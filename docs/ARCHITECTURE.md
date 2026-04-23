# Architecture Deep Dive

## Request flow

```
┌───────────────────────────────────────────────────────────────────┐
│                  Pod in team-a namespace                          │
│                  HTTP_PROXY=forward-proxy:3128                    │
│                                                                   │
│  curl http://example.com/path                                     │
│  → HTTP client sees HTTP_PROXY env var                            │
│  → sends: GET http://example.com/path HTTP/1.1  (absolute-form)   │
│  → to TCP: forward-proxy.istio-egress.svc:3128                    │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  Plain TCP (intra-cluster)
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│                  ztunnel on source node                           │
│                                                                   │
│  - intercepts traffic to the forward-proxy service                │
│  - wraps in HBONE: mTLS tunnel over port 15008                    │
│  - presents pod SPIFFE identity:                                  │
│    spiffe://cluster.local/ns/team-a/sa/app-x                      │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  HBONE mTLS (pod identity preserved)
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│                  ztunnel on destination node                      │
│                                                                   │
│  - decrypts HBONE                                                 │
│  - optionally sets SPIFFE URI in X-Forwarded-Client-Cert header   │
│  - delivers plain TCP to forward-proxy pod on port 3128           │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  Plain TCP (local on node)
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
│     - exact host map lookup                                       │
│     - wildcard fallback (*.example.com)                           │
│     - returns (true, entry) or (false, _)                         │
│                                                                   │
│  3. On deny: 403 + audit log, done                                │
│                                                                   │
│  4. Audit event                                                   │
│     {                                                             │
│       "ts": "...",                                                │
│       "spiffe": "spiffe://.../ns/team-a/sa/app-x",               │
│       "method": "HTTP-FORWARD",                                   │
│       "target_host": "example.com",                               │
│       "decision": "allow"                                         │
│     }                                                             │
│                                                                   │
│  5. Open mTLS connection to upstream                              │
│     - tls.Dial("tcp", "corporate-proxy:8080", tlsCfg)             │
│     - tlsCfg.Certificates = client cert from Secret              │
│     - tlsCfg.RootCAs = ca.crt from Secret                        │
│     - cert-manager rotates automatically                          │
│     - fsnotify detects file change, reloads without restart       │
│                                                                   │
│  6. Write request-line WITH ABSOLUTE PATH                         │
│     → conn.Write("GET http://example.com/path HTTP/1.1\r\n")     │
│     → headers:                                                    │
│        Host: example.com                                          │
│        Proxy-Authorization: Basic <base64>                        │
│        [custom headers from values.yaml]                          │
│        [end-to-end client headers, filtered]                      │
│                                                                   │
│  7. Read upstream response, copy to client                        │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  mTLS (client cert auth)
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│              corporate-proxy.corp:8080                            │
│                                                                   │
│  - validates client certificate against ca.crt                   │
│  - validates Proxy-Authorization                                  │
│  - sees absolute URI in request-line → knows destination          │
│  - forwards to next hop or directly                               │
└───────────────────────────────────────────────────────────────────┘
                          │
                          │  (TLS to upstream chain or plain HTTP)
                          ▼
                 [next proxies in chain]
                          │
                          ▼
┌───────────────────────────────────────────────────────────────────┐
│              external Squid (absolute-form parser) ✅             │
│                                                                   │
│  Receives: GET http://example.com/path HTTP/1.1                   │
│  Parses absolute URI correctly                                    │
│  Forwards to example.com                                          │
└───────────────────────────────────────────────────────────────────┘
```

## Threading model

Each incoming request spawns a goroutine via `http.Server`.

- `handleHTTPForward` is synchronous within the goroutine: ACL check, dial
  upstream, write request, read response. No extra goroutines.
- `handleConnect` spawns 2 goroutines for bidirectional tunnel copy
  (client→upstream and upstream→client), which end when either side closes.

Shared state is thread-safe:

- `serviceentry.Watcher` uses `sync.RWMutex` for the allowlist map
- `certs.Manager` uses `atomic.Pointer[tls.Config]` for lock-free reads
- Prometheus counters are thread-safe by design

## Failure modes

| Scenario | Behavior |
|---|---|
| ServiceEntry watcher not synced | `/readyz` returns 503, kubelet keeps pod out of Service |
| Cert file missing at startup | Pod crashes, kubelet restarts; pod only becomes Ready after success |
| Cert file corrupt during rotation | Old config remains active; error is logged |
| Upstream unreachable | 502 Bad Gateway to client, dial error counter incremented |
| ACL miss | 403 Forbidden, audit log with deny_reason |
| Client disconnect mid-forward | Goroutine exits cleanly via io.Copy |
| ztunnel failure | Client requests do not reach proxy; no impact on existing tunnels |

## Why not a GatewayClass controller?

For a **central platform-team proxy**, a single Deployment is sufficient. A
GatewayClass controller adds value when:

- Teams self-provision instances via their own `Gateway` objects
- Each instance needs a different configuration (upstream, ACL)
- A self-service model is required

With one shared proxy the Helm chart is the right abstraction.

## Why no sidecar injection?

The forward proxy pod runs **in ambient mesh** via the `dataplane-mode=ambient`
label, not via a sidecar. This means:

- ztunnel handles L4 mTLS for inbound traffic
- No Envoy sidecar overhead
- `PeerAuthentication: STRICT` enforces that traffic only arrives via ztunnel
- `AuthorizationPolicy` can enforce L4 (namespace/principal) and L7 (path/method)

## Why mTLS to upstream but not to the client?

- **Client → proxy**: already encrypted by ztunnel HBONE mTLS. Double-encrypting
  adds overhead without benefit.
- **Proxy → upstream**: the upstream is outside the mesh; HBONE is not available.
  Explicit mTLS with a client certificate is the only way to guarantee
  authenticity and confidentiality.

This mirrors the Istio TLS origination pattern from the
[DestinationRule MUTUAL](https://istio.io/latest/docs/tasks/traffic-management/egress/egress-tls-origination/#mutual-tls-origination-for-egress-traffic)
documentation.
