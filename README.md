# istio-forward-proxy

Een forward proxy voor **Istio ambient mode** die het gedrag van Istio's TLS
origination nabootst (ServiceEntry + DestinationRule met `credentialName`),
maar met één cruciaal verschil: **het behoudt het absolute pad** in HTTP
request-lines naar de upstream.

## Waarom bestaat dit?

Envoy (en dus ook de Istio-native TLS origination) herschrijft absolute URLs
naar relatieve paden bij het forwarden:

```
Envoy:     GET /politics HTTP/1.1          ← relatief, voor origin servers
Deze proxy: GET http://edition.cnn.com/politics HTTP/1.1   ← absoluut, voor proxy chains
```

Wanneer je achter Istio een **proxy chain** hebt staan (bijvoorbeeld een
corporate Squid die `absoluut pad` vereist), werkt Envoy niet. Deze proxy
wél.

## Architectuur

```
Pod (HTTP_PROXY=forward-proxy:3128)
 │  plain HTTP met absoluut pad
 ▼
ztunnel (HBONE mTLS, SPIFFE identiteit bewaard)
 ▼
istio-forward-proxy (deze repo)
 │  ACL check via ServiceEntry allowlist
 │  Audit log met SPIFFE identiteit
 │  mTLS origination met client certificaat
 ▼
upstream webproxy chain
 ▼
externe Squid (ontvangt absoluut pad ✅)
 ▼
externe bestemming
```

## Features

| Feature | Beschrijving |
|---|---|
| Absolute path forwarding | `GET http://host/path HTTP/1.1` naar upstream |
| CONNECT tunnel support | Voor HTTPS verkeer met upstream auth |
| mTLS origination | Client cert via cert-manager, hot-reload bij rotatie |
| ServiceEntry-gebaseerde ACL | Alleen geregistreerde hosts doorgelaten |
| SPIFFE audit logging | Pod identiteit per verbinding in de log |
| Proxy-Authorization injectie | Credentials naar upstream chain |
| Custom header manipulatie | Via `extraHeaders` in values.yaml |
| Prometheus metrics | `/metrics` endpoint met counters en gauges |
| HPA + PDB | Horizontaal schalen met graceful disruption |
| Istio ambient integratie | `dataplane-mode=ambient` label, PeerAuthentication STRICT |

## Projectstructuur

```
.
├── cmd/                             Entrypoint (main.go)
├── internal/
│   ├── audit/                      JSON audit events
│   ├── certs/                      mTLS cert loader + fsnotify reload
│   ├── proxy/                      Core forward proxy + CONNECT handler
│   └── serviceentry/               Kubernetes informer voor ACL
├── deploy/
│   ├── docker/Dockerfile           Multi-stage distroless build
│   ├── helm/istio-forward-proxy/   Helm chart
│   └── istio/                      Namespace + voorbeeld ServiceEntry
├── scripts/
│   ├── build.sh                    Docker image bouwen + pushen
│   ├── install.sh                  Helm install wrapper
│   ├── e2e-test.sh                 End-to-end cluster tests
│   ├── verify-absolute-path.sh     Bewijs dat paden absoluut blijven
│   └── local-test.sh               Lokale smoke test zonder cluster
├── test/                            Test fixtures
└── docs/                            Aanvullende documentatie
```

## Installatie

### 1. Build de Docker image

```bash
export IMAGE_REPO=registry.corp.local/platform/istio-forward-proxy
export IMAGE_TAG=0.1.0
./scripts/build.sh

# Of met push naar registry:
PUSH=1 ./scripts/build.sh
```

### 2. Configureer je `values.yaml`

De belangrijkste parameters:

```yaml
image:
  repository: registry.corp.local/platform/istio-forward-proxy
  tag: "0.1.0"

proxy:
  upstream:
    # Je eerste upstream in de webproxy chain
    host: "corporate-proxy.intern.corp:8080"
    # Ofwel direct, ofwel via Secret (voorkeur)
    authSecretRef:
      name: upstream-proxy-auth
      key: proxy-auth

  mtls:
    enabled: true
    certManager:
      enabled: true
      issuerRef:
        name: corp-internal-ca
        kind: ClusterIssuer

networkPolicies:
  egressFromProxy:
    upstreamCIDRs:
      - 10.20.0.0/16   # Jouw upstream proxy CIDR

istio:
  authorizationPolicy:
    allowedNamespaces:
      - "*"   # Central shared proxy
```

### 3. Installeer

```bash
./scripts/install.sh
```

Of handmatig:

```bash
kubectl apply -f deploy/istio/00-namespace.yaml
helm install istio-forward-proxy ./deploy/helm/istio-forward-proxy \
  --namespace istio-egress \
  --values my-values.yaml
```

### 4. Registreer externe hosts

Elk team registreert zelf welke externe hosts hun pods mogen bereiken, in
hun eigen namespace:

```yaml
apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: example-com
  namespace: team-a
spec:
  hosts:
    - example.com
  ports:
    - number: 443
      name: https
      protocol: HTTPS
    - number: 80
      name: http
      protocol: HTTP
  resolution: DNS
  location: MESH_EXTERNAL
```

### 5. Pods configureren

Elke pod die de proxy moet gebruiken zet `HTTP_PROXY`:

```yaml
env:
  - name: HTTP_PROXY
    value: "http://istio-forward-proxy.istio-egress.svc.cluster.local:3128"
  - name: HTTPS_PROXY
    value: "http://istio-forward-proxy.istio-egress.svc.cluster.local:3128"
  - name: NO_PROXY
    value: ".svc.cluster.local,.cluster.local,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
```

## Testen

### Lokale unit tests (zonder cluster)

```bash
cd internal/proxy && go test -v
```

De meest kritische test is `TestAbsolutePathPreservation` — deze bewijst dat
de proxy **niet** doet wat Envoy doet.

### End-to-end cluster test

```bash
./scripts/e2e-test.sh
```

Dit test:
- Pod kan niet-geregistreerde hosts niet bereiken (403)
- ServiceEntry registratie maakt host bereikbaar
- Metrics endpoint levert data
- Audit log bevat structured events

### Bewijs van absolute pad (de belangrijkste test)

```bash
./scripts/verify-absolute-path.sh
```

Dit script deployt een mock upstream die raw request-lines logt, stuurt
verkeer door de proxy, en faalt als er een relatief pad binnenkomt. Dit is
het definitieve bewijs dat deze proxy zich anders gedraagt dan Envoy.

## Operationele concerns

### Certificaat rotatie

cert-manager roteert het client certificaat periodiek. De proxy detecteert
dit automatisch via `fsnotify` op de gemount Secret volume en laadt de nieuwe
TLS config zonder restart.

### Horizontaal schalen

De HPA is standaard aan. Houd rekening met:

- **CONNECT tunnels hebben lokale state.** Bij scale-down worden open
  verbindingen verbroken. Applicaties moeten retry-logic hebben.
- `scaleDown.stabilizationWindowSeconds: 300` geeft een rustige afbouw.
- `PodDisruptionBudget.minAvailable: 1` voorkomt totale uitval tijdens
  rolling updates.

### Monitoring

Prometheus metrics zijn beschikbaar op `:9090/metrics`:

| Metric | Type | Labels |
|---|---|---|
| `forward_proxy_requests_total` | counter | method, decision |
| `forward_proxy_active_connections` | gauge | — |
| `forward_proxy_upstream_dial_errors_total` | counter | — |
| `forward_proxy_bytes_transferred_total` | counter | direction |

Een `ServiceMonitor` is beschikbaar via `values.yaml`:

```yaml
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack
```

### Debugging

```bash
# Current allowlist
kubectl -n istio-egress port-forward svc/istio-forward-proxy 9090:9090
curl http://localhost:9090/debug/allowlist

# Live audit log
kubectl -n istio-egress logs -f deployment/istio-forward-proxy \
  | jq 'select(.component == "audit")'

# Detailed trace
kubectl -n istio-egress set env deployment/istio-forward-proxy LOG_LEVEL=debug
```

## Relatie tot de Istio TLS origination doc

De officiele Istio task
[Egress TLS Origination](https://istio.io/latest/docs/tasks/traffic-management/egress/egress-tls-origination/)
beschrijft de standaard aanpak met `ServiceEntry` + `DestinationRule` +
`credentialName`. Deze repo implementeert hetzelfde gedrag met één verschil:

| Istio + Envoy | Deze proxy |
|---|---|
| `ServiceEntry` definieert toegestane hosts | ServiceEntry watcher bouwt ACL |
| `DestinationRule` met `tls.mode: MUTUAL` | mTLS dial naar upstream |
| `credentialName` verwijst naar Secret met tls.key/crt, ca.crt | Identieke Secret structuur, via cert-manager |
| Request naar upstream: **relatief pad** | Request naar upstream: **absoluut pad** |
| ServiceEntry `targetPort: 443` doet port mapping | Go code doet dezelfde mapping |

## Uitbreidingen

Mogelijke toekomstige features:

- Rate limiting per SPIFFE identiteit
- Circuit breaker naar upstream
- OpenTelemetry tracing
- Multi-upstream met health-based failover
- WASM plugin voor custom header logic

## Licentie

Apache 2.0
