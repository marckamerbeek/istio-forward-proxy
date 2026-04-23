# istio-forward-proxy

A forward proxy for **Istio ambient mode** that preserves absolute paths in
HTTP request-lines when forwarding to an upstream proxy chain.

Envoy's TLS origination (via `ServiceEntry` + `DestinationRule`) rewrites
absolute URLs to relative paths, breaking downstream proxies such as Squid
that require absolute-form requests:

```
Envoy:                GET /politics HTTP/1.1                        ← origin-server form
istio-forward-proxy:  GET http://edition.cnn.com/politics HTTP/1.1  ← proxy form (RFC 7230 §5.3.2)
```

## Architecture

```
Pod (HTTP_PROXY=forward-proxy:3128)
 │  plain HTTP with absolute path
 ▼
ztunnel (HBONE mTLS, SPIFFE identity preserved)
 ▼
istio-forward-proxy (this repo)
 │  ACL check via ServiceEntry allowlist
 │  Audit log with SPIFFE identity
 │  mTLS origination with client certificate
 ▼
upstream proxy chain
 ▼
external Squid (receives absolute path ✅)
 ▼
external destination
```

## Features

| Feature | Description |
|---|---|
| Absolute path forwarding | `GET http://host/path HTTP/1.1` to upstream |
| CONNECT tunnel support | For HTTPS traffic with upstream auth |
| mTLS origination | Client cert via cert-manager, hot-reload on rotation |
| ServiceEntry-based ACL | Only registered hosts are allowed through |
| SPIFFE audit logging | Pod identity per connection in structured logs |
| Proxy-Authorization injection | Credentials forwarded to upstream chain |
| Custom header injection | Via `extraHeaders` in values.yaml |
| Prometheus metrics | `/metrics` endpoint with counters and gauges |
| HPA + PDB | Horizontal scaling with graceful disruption |
| Istio ambient integration | `dataplane-mode=ambient` label, PeerAuthentication STRICT |

## Project layout

```
.
├── cmd/                             Entrypoint (main.go)
├── internal/
│   ├── audit/                       Structured JSON audit events
│   ├── certs/                       mTLS cert loader + fsnotify hot-reload
│   ├── proxy/                       Core forward proxy + CONNECT handler
│   └── serviceentry/                Kubernetes informer for ACL
├── deploy/
│   ├── docker/Dockerfile            Multi-stage distroless build
│   ├── helm/istio-forward-proxy/    Helm chart
│   └── istio/                       Namespace + example ServiceEntry
├── scripts/
│   ├── build.sh                     Build and push Docker image
│   ├── install.sh                   Helm install wrapper
│   ├── e2e-test.sh                  End-to-end cluster tests
│   ├── verify-absolute-path.sh      Proof that paths stay absolute
│   └── local-test.sh                Local smoke test without a cluster
└── docs/                            Additional documentation
```

## Installation

### 1. Build the Docker image

```bash
export IMAGE_REPO=your-registry/istio-forward-proxy
export IMAGE_TAG=0.1.0
./scripts/build.sh

# Build and push:
PUSH=1 ./scripts/build.sh
```

### 2. Configure `values.yaml`

Key parameters:

```yaml
image:
  repository: your-registry/istio-forward-proxy
  tag: "0.1.0"

proxy:
  upstream:
    host: "corporate-proxy.corp.local:8080"
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
      - 10.20.0.0/16

istio:
  authorizationPolicy:
    allowedNamespaces:
      - "*"
```

### 3. Install

```bash
./scripts/install.sh
```

Or manually:

```bash
kubectl apply -f deploy/istio/00-namespace.yaml
helm install istio-forward-proxy ./deploy/helm/istio-forward-proxy \
  --namespace istio-egress \
  --values my-values.yaml
```

### 4. Register external hosts

Each team registers which external hosts their pods may reach in their own namespace:

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

### 5. Configure pods

```yaml
env:
  - name: HTTP_PROXY
    value: "http://istio-forward-proxy.istio-egress.svc.cluster.local:3128"
  - name: HTTPS_PROXY
    value: "http://istio-forward-proxy.istio-egress.svc.cluster.local:3128"
  - name: NO_PROXY
    value: ".svc.cluster.local,.cluster.local,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
```

## Testing

### Unit tests

```bash
cd internal/proxy && go test -v
```

`TestAbsolutePathPreservation` is the critical test — it proves the proxy
preserves the absolute URI that Envoy would rewrite.

### End-to-end cluster test

```bash
./scripts/e2e-test.sh
```

Tests: ACL deny for unregistered hosts (403), allow after ServiceEntry
registration, metrics endpoint, and structured audit events.

### Absolute path verification

```bash
./scripts/verify-absolute-path.sh
```

Deploys a mock upstream that logs raw request-lines, sends traffic through
the proxy, and fails if a relative path is received.

## Operations

See [docs/OPERATIONS.md](docs/OPERATIONS.md) for the full operations runbook.

### Certificate rotation

cert-manager rotates the client certificate periodically. The proxy detects
changes via `fsnotify` on the mounted Secret volume and reloads TLS config
without a restart.

### Metrics

Prometheus metrics on `:9090/metrics`:

| Metric | Type | Labels |
|---|---|---|
| `forward_proxy_requests_total` | counter | method, decision |
| `forward_proxy_active_connections` | gauge | — |
| `forward_proxy_upstream_dial_errors_total` | counter | — |
| `forward_proxy_bytes_transferred_total` | counter | direction |

### Debugging

```bash
# Current allowlist
kubectl -n istio-egress port-forward svc/istio-forward-proxy 9090:9090
curl http://localhost:9090/debug/allowlist

# Live audit log
kubectl -n istio-egress logs -f deployment/istio-forward-proxy \
  | jq 'select(.component == "audit")'

# Increase log verbosity
kubectl -n istio-egress set env deployment/istio-forward-proxy LOG_LEVEL=debug
```

## Comparison with Istio TLS origination

The official Istio
[Egress TLS Origination](https://istio.io/latest/docs/tasks/traffic-management/egress/egress-tls-origination/)
task uses `ServiceEntry` + `DestinationRule` + `credentialName`. This proxy
implements the same pattern with one difference:

| Istio + Envoy | This proxy |
|---|---|
| `ServiceEntry` defines allowed hosts | ServiceEntry watcher builds ACL |
| `DestinationRule` with `tls.mode: MUTUAL` | mTLS dial to upstream |
| `credentialName` references a Secret | Same Secret structure, via cert-manager |
| Request to upstream: **relative path** | Request to upstream: **absolute path** |

## License

Apache 2.0 — see [LICENSE](LICENSE).
