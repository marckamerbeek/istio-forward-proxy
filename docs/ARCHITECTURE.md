# Architecture Deep Dive

## De flow in detail

```
┌───────────────────────────────────────────────────────────────────┐
│                  Pod in team-a namespace                          │
│                  HTTP_PROXY=forward-proxy:3128                    │
│                                                                   │
│  curl http://example.com/path                                     │
│  → client library ziet HTTP_PROXY env var                         │
│  → stuurt: GET http://example.com/path HTTP/1.1  (absolute-form)  │
│  → naar TCP: forward-proxy.istio-egress.svc:3128                  │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  Plain TCP (intra-cluster)
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│                  ztunnel op source node                            │
│                                                                   │
│  - onderschept verkeer richting forward-proxy service              │
│  - wrapped in HBONE: mTLS tunnel over poort 15008                  │
│  - presenteert SPIFFE identity van pod:                            │
│    spiffe://cluster.local/ns/team-a/sa/app-x                       │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  HBONE mTLS (pod identity doorgezet)
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│                  ztunnel op destination node                       │
│                                                                   │
│  - decrypteert HBONE                                               │
│  - plaatst SPIFFE URI in header X-Forwarded-Client-Cert (optioneel) │
│  - levert plain TCP aan forward-proxy pod op poort 3128            │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  Plain TCP (lokaal op node)
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│              istio-forward-proxy pod                              │
│                                                                   │
│  1. Parse HTTP request                                            │
│     - method = GET                                                │
│     - r.URL = http://example.com/path  (absolute URI)             │
│     - r.Host = example.com                                        │
│                                                                   │
│  2. ACL check                                                     │
│     serviceentry.Watcher.AllowHost("example.com", 80)             │
│     - zoekt in exacte hosts map                                   │
│     - valt terug op wildcard matches (*.example.com)              │
│     - return (true, entry) of (false, _)                          │
│                                                                   │
│  3. Indien deny: 403 + audit log, einde                           │
│                                                                   │
│  4. Audit event                                                   │
│     {                                                             │
│       "ts": "2026-04-22T...",                                     │
│       "spiffe": "spiffe://.../ns/team-a/sa/app-x",                │
│       "method": "HTTP-FORWARD",                                   │
│       "target_host": "example.com",                               │
│       "decision": "allow"                                         │
│     }                                                             │
│                                                                   │
│  5. Open mTLS verbinding naar upstream                            │
│     - tls.Dial("tcp", "corporate-proxy:8080", tlsCfg)             │
│     - tlsCfg bevat Certificates = client cert uit Secret          │
│     - tlsCfg.RootCAs = ca.crt uit Secret                          │
│     - cert-manager roteert automatisch                            │
│     - fsnotify detecteert file change, reload zonder restart      │
│                                                                   │
│  6. Schrijf request-line MET ABSOLUUT PAD                         │
│     → conn.Write("GET http://example.com/path HTTP/1.1\r\n")      │
│     → headers:                                                     │
│        Host: example.com                                           │
│        Proxy-Authorization: Basic <base64>                         │
│        [custom headers uit values.yaml]                           │
│        [end-to-end client headers, filtered]                      │
│                                                                   │
│  7. Lees upstream response, kopieer naar client                   │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  mTLS (client cert auth)
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│              corporate-proxy.intern:8080                           │
│                                                                   │
│  - valideert client certificaat tegen ca.crt                      │
│  - valideert Proxy-Authorization                                  │
│  - ziet absolute URI in request-line → weet bestemming            │
│  - forward naar volgende hop of rechtstreeks                      │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  (TLS naar upstream chain of plain HTTP)
                          ▼
                 [volgende proxies in chain]
                          │
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│              externe Squid (absolute-form parser) ✅               │
│                                                                   │
│  Krijgt: GET http://example.com/path HTTP/1.1                     │
│  Parseert absolute URI correct                                    │
│  Forward naar example.com                                         │
└───────────────────────────────────────────────────────────────────┘
```

## Threading model

De Go runtime gebruikt M:N scheduling. Elk inkomend request spawnt een
goroutine via `http.Server`. Binnen die goroutine:

- `handleHTTPForward` is synchronous van binnen: lees ACL, dial upstream,
  schrijf request, lees response. Geen extra goroutines behalve die `net/http`
  intern gebruikt.
- `handleConnect` spawnt 2 goroutines voor bidirectionele tunnel kopie
  (client→upstream en upstream→client). Deze eindigen als één kant EOF/close
  krijgt.

Gedeelde state is thread-safe:

- `serviceentry.Watcher` gebruikt `sync.RWMutex` voor de allowlist map
- `certs.Manager` gebruikt `atomic.Pointer[tls.Config]` voor lock-vrije
  reads
- Prometheus counters zijn thread-safe by design

## Fail-modes

| Scenario | Gedrag |
|---|---|
| ServiceEntry watcher niet synced | `/readyz` returnt 503, kubelet houdt pod uit Service |
| Cert file ontbreekt bij start | Pod crasht, kubelet restart; pas na succes wordt pod Ready |
| Cert file corrupt tijdens rotatie | Oude config blijft actief; error wordt gelogd |
| Upstream onbereikbaar | 502 Bad Gateway naar client, metric counter +1 |
| ACL miss | 403 Forbidden, audit log met deny_reason |
| Client disconnect midden in forward | Goroutine eindigt clean door io.Copy |
| ztunnel faalt | Client requests halen proxy niet, geen impact op bestaande tunnels |

## Waarom geen GatewayClass controller?

De originele discussie ging uit van een custom GatewayClass. Voor een
**centrale platform-team proxy** is dat over-engineering: één Deployment
volstaat. Een GatewayClass controller is waardevol als:

- Teams zelf instanties provisioneren via hun eigen `Gateway` objects
- Elke instantie andere configuratie moet hebben (andere upstream, andere ACL)
- Er een zelfbediening-model nodig is

Bij één gedeelde proxy voegt de GatewayClass laag complexiteit toe zonder
waarde. De Helm chart is de juiste abstractie hier.

## Waarom geen sidecar injection?

De forward proxy pod draait **in ambient mesh** via `dataplane-mode=ambient`
label, niet via sidecar. Dit betekent:

- Ztunnel handelt L4 mTLS af voor inkomend verkeer
- Geen Envoy sidecar overhead
- `PeerAuthentication: STRICT` dwingt af dat verkeer alleen via ztunnel komt
- `AuthorizationPolicy` kan L4 (namespace/principal) + L7 (path/method) zijn
- Voor L7 regels op de proxy zelf zou een waypoint nodig zijn (niet
  geconfigureerd want niet vereist)

## Waarom mTLS met upstream en niet met client?

- **Client → proxy**: al versleuteld door ztunnel HBONE mTLS. Dubbel doen
  is overhead zonder winst.
- **Proxy → upstream**: upstream zit buiten de mesh, HBONE is daar niet.
  Expliciete mTLS met eigen client cert is de enige manier om authenticiteit
  en vertrouwelijkheid te garanderen.

Dit komt exact overeen met de Istio TLS origination pattern uit de
[DestinationRule MUTUAL](https://istio.io/latest/docs/tasks/traffic-management/egress/egress-tls-origination/#mutual-tls-origination-for-egress-traffic)
documentatie.
