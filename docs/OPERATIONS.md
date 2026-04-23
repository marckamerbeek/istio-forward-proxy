# Operations Runbook

## Day-2 operations for istio-forward-proxy

### Daily health check

```bash
kubectl -n istio-egress get pods,hpa,pdb
kubectl -n istio-egress top pods
```

### Audit log queries

Allow events from the last 15 minutes:

```bash
kubectl -n istio-egress logs deployment/istio-forward-proxy --since=15m \
  | jq 'select(.decision == "allow") | {ts, spiffe, target_host, bytes_in, bytes_out}'
```

Deny events:

```bash
kubectl -n istio-egress logs deployment/istio-forward-proxy --since=1h \
  | jq 'select(.decision == "deny") | {ts, spiffe, target_host, deny_reason}'
```

Top 10 destinations:

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

Deny rate (possible misconfiguration):

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

### Symptom: all requests return 403

**Cause**: ServiceEntry cache is not synced, or no ServiceEntries exist.

**Verify**:

```bash
# Check allowlist
kubectl -n istio-egress exec deploy/istio-forward-proxy -- \
  wget -q -O - http://localhost:9090/debug/allowlist

# Check readiness
kubectl -n istio-egress exec deploy/istio-forward-proxy -- \
  wget -q -O - http://localhost:9090/readyz
```

**Fix**:

- Empty allowlist: check RBAC on ClusterRole/ClusterRoleBinding
- Check ServiceEntries exist: `kubectl get serviceentries -A`
- Restart proxy pods to force a fresh list

### Symptom: all requests return 502 Bad Gateway

**Cause**: upstream proxy unreachable or TLS mismatch.

**Verify**:

```bash
# Check upstream dial errors
kubectl -n istio-egress exec deploy/istio-forward-proxy -- \
  wget -q -O - http://localhost:9090/metrics | grep upstream_dial

# Check certificate validity
kubectl -n istio-egress get certificate
kubectl -n istio-egress describe certificate istio-forward-proxy-client

# Test from proxy pod (distroless has no shell, use an ephemeral debug container):
kubectl -n istio-egress debug -it deploy/istio-forward-proxy \
  --image=nicolaka/netshoot \
  --target=proxy \
  -- /bin/sh
# Inside the container:
#   openssl s_client -connect corporate-proxy.corp:8080 \
#     -cert /etc/proxy/certs/tls.crt \
#     -key /etc/proxy/certs/tls.key \
#     -CAfile /etc/proxy/certs/ca.crt
```

**Fix**:

- Expired cert: check cert-manager, `kubectl -n istio-egress annotate certificate istio-forward-proxy-client cert-manager.io/issue-temporary-certificate=true --overwrite`
- Upstream hostname not resolving: check CoreDNS, check NetworkPolicy egressFromProxy
- Upstream unreachable: check firewall / NetworkPolicy

### Symptom: intermittent 502s under load

**Cause**: HPA scaling event broke connections, or connection leak.

**Verify**:

```bash
kubectl -n istio-egress describe hpa istio-forward-proxy
kubectl -n istio-egress get events --sort-by='.lastTimestamp' | tail -20
```

**Fix**:

- Increase `autoscaling.minReplicas` to maintain a buffer
- Increase `podDisruptionBudget.minAvailable`
- Ensure applications have retry logic; TCP connections are stateful

### Symptom: certificate not reloaded after rotation

**Cause**: fsnotify event missed, or file permission mismatch.

**Verify**:

```bash
kubectl -n istio-egress logs deployment/istio-forward-proxy \
  | grep -E 'cert|reload|load.*keypair'
```

**Fix**:

- Force reload: `kubectl -n istio-egress rollout restart deployment/istio-forward-proxy`
- Check Secret permissions: `defaultMode: 0400` in Deployment volumes
- Verify Secret keys are named exactly `tls.crt`, `tls.key`, `ca.crt`

## Upgrades

### Minor version (image tag)

```bash
helm upgrade istio-forward-proxy ./deploy/helm/istio-forward-proxy \
  --namespace istio-egress \
  --reuse-values \
  --set image.tag=0.2.0
```

Rolling update keeps at least `minAvailable` replicas running.

### Chart upgrade with schema changes

Check the chart CHANGELOG first, test in staging, then:

```bash
helm diff upgrade istio-forward-proxy ./deploy/helm/istio-forward-proxy \
  -n istio-egress -f prod-values.yaml

helm upgrade istio-forward-proxy ./deploy/helm/istio-forward-proxy \
  -n istio-egress -f prod-values.yaml
```

### Istio ambient upgrade

The proxy is a regular pod in ambient mesh. After an Istio upgrade:

1. `istioctl x precheck`
2. Upgrade istiod + ztunnel via Helm
3. Restart the forward proxy: `kubectl -n istio-egress rollout restart deployment/istio-forward-proxy`
4. Run e2e tests

## Capacity planning

A single pod (100m/128Mi request) handles approximately:

- 500 req/s for short HTTP requests
- 50 MB/s throughput for CONNECT tunnels
- ~1000 concurrent open connections

HPA scales at 70% CPU utilization. For 10k concurrent tunnels plan for ~20 replicas.
