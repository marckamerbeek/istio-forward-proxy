# Operations Runbook

## Day-2 operations voor istio-forward-proxy

### Dagelijkse health check

```bash
kubectl -n istio-egress get pods,hpa,pdb
kubectl -n istio-egress top pods
```

### Audit log query

Alle allow events van de laatste 15 minuten:

```bash
kubectl -n istio-egress logs deployment/istio-forward-proxy --since=15m \
  | jq 'select(.decision == "allow") | {ts, spiffe, target_host, bytes_in, bytes_out}'
```

Alle deny events:

```bash
kubectl -n istio-egress logs deployment/istio-forward-proxy --since=1h \
  | jq 'select(.decision == "deny") | {ts, spiffe, target_host, deny_reason}'
```

Top 10 bestemmingen:

```bash
kubectl -n istio-egress logs deployment/istio-forward-proxy --since=1h \
  | jq -r 'select(.decision == "allow") | .target_host' \
  | sort | uniq -c | sort -rn | head -10
```

### Prometheus queries

Error rate:

```promql
rate(forward_proxy_requests_total{decision="upstream_error"}[5m])
```

Deny rate (mogelijk een misconfiguratie):

```promql
rate(forward_proxy_requests_total{decision="deny"}[5m])
```

P95 bandwidth:

```promql
rate(forward_proxy_bytes_transferred_total[5m])
```

Active connections per pod:

```promql
forward_proxy_active_connections
```

## Incident response

### Symptoom: alle requests krijgen 403

**Oorzaak**: ServiceEntry cache is niet synced, of er zijn geen ServiceEntries.

**Verificatie**:

```bash
# Check allowlist
kubectl -n istio-egress exec deploy/istio-forward-proxy -- \
  wget -q -O - http://localhost:9090/debug/allowlist

# Check readiness
kubectl -n istio-egress exec deploy/istio-forward-proxy -- \
  wget -q -O - http://localhost:9090/readyz
```

**Fix**:

- Als allowlist leeg is: check RBAC op ClusterRole/ClusterRoleBinding
- Check dat ServiceEntries bestaan: `kubectl get serviceentries -A`
- Restart proxy pods om een fresh list te forceren

### Symptoom: alle requests krijgen 502 Bad Gateway

**Oorzaak**: upstream proxy onbereikbaar of TLS mismatch.

**Verificatie**:

```bash
# Check upstream dial errors
kubectl -n istio-egress exec deploy/istio-forward-proxy -- \
  wget -q -O - http://localhost:9090/metrics | grep upstream_dial

# Check cert geldig
kubectl -n istio-egress get certificate
kubectl -n istio-egress describe certificate istio-forward-proxy-client

# Test vanaf proxy pod (als shell beschikbaar; distroless heeft geen shell!)
# Dus gebruik ephemeral debug container:
kubectl -n istio-egress debug -it deploy/istio-forward-proxy \
  --image=nicolaka/netshoot \
  --target=proxy \
  -- /bin/sh
# In de container:
#   openssl s_client -connect corporate-proxy.intern:8080 \
#     -cert /etc/proxy/certs/tls.crt \
#     -key /etc/proxy/certs/tls.key \
#     -CAfile /etc/proxy/certs/ca.crt
```

**Fix**:

- Cert verlopen: cert-manager check, `kubectl -n istio-egress annotate certificate istio-forward-proxy-client cert-manager.io/issue-temporary-certificate=true --overwrite`
- Upstream hostname resolution faalt: check CoreDNS, check NetworkPolicy egressFromProxy
- Upstream niet bereikbaar: firewall / NetworkPolicy

### Symptoom: intermittente 502s onder load

**Oorzaak**: HPA scaling event brak verbindingen, of connection leak.

**Verificatie**:

```bash
kubectl -n istio-egress describe hpa istio-forward-proxy
kubectl -n istio-egress get events --sort-by='.lastTimestamp' | tail -20
```

**Fix**:

- Verhoog `autoscaling.minReplicas` om buffer te hebben
- Verhoog `podDisruptionBudget.minAvailable`
- Check of applicaties retry-logic hebben; TCP verbindingen zijn stateful

### Symptoom: cert niet geladen na rotatie

**Oorzaak**: fsnotify event gemist, of file permission mismatch.

**Verificatie**:

```bash
kubectl -n istio-egress logs deployment/istio-forward-proxy \
  | grep -E 'cert|reload|load.*keypair'
```

**Fix**:

- Force reload: `kubectl -n istio-egress rollout restart deployment/istio-forward-proxy`
- Check Secret permissions: `defaultMode: 0400` in Deployment volumes
- Check dat Secret keys `tls.crt`, `tls.key`, `ca.crt` heten exact

## Upgrades

### Minor version (image tag)

```bash
helm upgrade istio-forward-proxy ./deploy/helm/istio-forward-proxy \
  --namespace istio-egress \
  --reuse-values \
  --set image.tag=0.2.0
```

Rolling update behoudt minimaal `minAvailable` replicas.

### Chart upgrade met schema changes

Check eerst de chart CHANGELOG, test in staging, dan:

```bash
helm diff upgrade istio-forward-proxy ./deploy/helm/istio-forward-proxy \
  -n istio-egress -f prod-values.yaml

helm upgrade istio-forward-proxy ./deploy/helm/istio-forward-proxy \
  -n istio-egress -f prod-values.yaml
```

### Istio ambient upgrade

De proxy is een normale pod in ambient mesh. Na een Istio upgrade:

1. `istioctl x precheck`
2. Upgrade istiod + ztunnel via Helm
3. Restart de forward proxy: `kubectl -n istio-egress rollout restart deployment/istio-forward-proxy`
4. Run e2e tests

## Capacity planning

Een enkele pod (100m/128Mi request) haalt bij benadering:

- 500 req/s voor korte HTTP requests
- 50 MB/s throughput voor CONNECT tunnels
- ~1000 concurrent open connections

Scale gaat mee met CPU: bij 70% CPU usage triggert HPA scale-up. Voor 10k
concurrent tunnels plan je op ~20 replicas.
