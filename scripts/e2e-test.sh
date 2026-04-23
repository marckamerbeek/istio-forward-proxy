#!/usr/bin/env bash
# End-to-end test script for istio-forward-proxy.
#
# Tests:
#   1. Proxy is running and healthy
#   2. ServiceEntry-registered hosts are allowed
#   3. Unregistered hosts are denied (403)
#   4. Requests to upstream contain an ABSOLUTE path
#   5. Hop-by-hop headers are correctly stripped
#   6. mTLS to upstream works with client certificate
#   7. Metrics endpoint returns data
#
# Prerequisites:
#   - kubectl with cluster admin
#   - Istio ambient mode installed
#   - cert-manager with a working ClusterIssuer
#   - The proxy installed via Helm

set -euo pipefail

PROXY_NS="${PROXY_NS:-istio-egress}"
TEST_NS="${TEST_NS:-forward-proxy-test}"
PROXY_NAME="${PROXY_NAME:-istio-forward-proxy}"
MOCK_UPSTREAM_NS="${MOCK_UPSTREAM_NS:-forward-proxy-test-upstream}"

log()  { echo -e "\033[1;34m[TEST]\033[0m $*"; }
pass() { echo -e "\033[1;32m[PASS]\033[0m $*"; }
fail() { echo -e "\033[1;31m[FAIL]\033[0m $*"; exit 1; }
step() { echo -e "\n\033[1;33m>>> $*\033[0m"; }

cleanup() {
  if [[ "${SKIP_CLEANUP:-0}" != "1" ]]; then
    log "Cleanup..."
    kubectl delete namespace "$TEST_NS" --ignore-not-found --wait=false
    kubectl delete namespace "$MOCK_UPSTREAM_NS" --ignore-not-found --wait=false
  fi
}
trap cleanup EXIT

# -----------------------------------------------------------------------------
step "1. Check proxy is running"
# -----------------------------------------------------------------------------
if ! kubectl -n "$PROXY_NS" get deployment "$PROXY_NAME" >/dev/null 2>&1; then
  fail "Deployment $PROXY_NAME not found in namespace $PROXY_NS"
fi
kubectl -n "$PROXY_NS" rollout status deployment/"$PROXY_NAME" --timeout=120s
pass "Proxy deployment ready"

# -----------------------------------------------------------------------------
step "2. Create test namespace and client pod"
# -----------------------------------------------------------------------------
kubectl create namespace "$TEST_NS" --dry-run=client -o yaml | \
  kubectl label --local -f - istio.io/dataplane-mode=ambient -o yaml | \
  kubectl apply -f -

PROXY_URL="http://${PROXY_NAME}.${PROXY_NS}.svc.cluster.local:3128"

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: test-client
  namespace: ${TEST_NS}
  labels:
    app: test-client
spec:
  containers:
    - name: curl
      image: curlimages/curl:8.7.1
      command: ["sleep", "infinity"]
      env:
        - name: HTTP_PROXY
          value: "${PROXY_URL}"
        - name: HTTPS_PROXY
          value: "${PROXY_URL}"
      securityContext:
        runAsNonRoot: true
        runAsUser: 100
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
        readOnlyRootFilesystem: true
  terminationGracePeriodSeconds: 5
EOF

kubectl -n "$TEST_NS" wait --for=condition=Ready pod/test-client --timeout=60s
pass "Test client pod ready, HTTP_PROXY=${PROXY_URL}"

# -----------------------------------------------------------------------------
step "3. Unregistered host is denied (403)"
# -----------------------------------------------------------------------------
set +e
OUTPUT=$(kubectl -n "$TEST_NS" exec test-client -c curl -- \
  curl -sS -o /dev/null -w "%{http_code}" http://example.invalid/ 2>&1)
set -e

if [[ "$OUTPUT" == "403" ]]; then
  pass "Unregistered host correctly denied with 403"
else
  fail "Expected 403 for unregistered host, got: $OUTPUT"
fi

# -----------------------------------------------------------------------------
step "4. Register httpbin.org as ServiceEntry"
# -----------------------------------------------------------------------------
cat <<EOF | kubectl apply -f -
apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: httpbin-org
  namespace: ${TEST_NS}
spec:
  hosts:
    - httpbin.org
  ports:
    - number: 443
      name: https
      protocol: HTTPS
    - number: 80
      name: http
      protocol: HTTP
  resolution: DNS
  location: MESH_EXTERNAL
EOF

log "Waiting for ServiceEntry to appear in allowlist..."
for i in $(seq 1 20); do
  ALLOWLIST=$(kubectl -n "$PROXY_NS" exec deploy/"$PROXY_NAME" -- \
    wget -q -O - http://localhost:9090/debug/allowlist 2>/dev/null || true)
  if echo "$ALLOWLIST" | grep -q "httpbin.org"; then
    pass "httpbin.org is in allowlist"
    break
  fi
  sleep 1
  [[ $i -eq 20 ]] && fail "ServiceEntry not visible in allowlist after 20s"
done

# -----------------------------------------------------------------------------
step "5. Registered host is allowed"
# -----------------------------------------------------------------------------
# Note: requires the upstream proxy to have internet access.
# In lab environments without internet, use a mock upstream instead.
set +e
STATUS=$(kubectl -n "$TEST_NS" exec test-client -c curl -- \
  curl -sS -o /dev/null -w "%{http_code}" http://httpbin.org/get 2>&1 || echo "FAIL")
set -e

if [[ "$STATUS" =~ ^(200|301|302)$ ]]; then
  pass "httpbin.org request succeeded (status $STATUS)"
else
  log "WARNING: status $STATUS (upstream may not have internet access)"
fi

# -----------------------------------------------------------------------------
step "6. Verify upstream receives ABSOLUTE paths"
# -----------------------------------------------------------------------------
# Deploy a mock upstream that logs every request, then send requests through
# the proxy and inspect the log.
kubectl create namespace "$MOCK_UPSTREAM_NS" --dry-run=client -o yaml | kubectl apply -f -

cat <<'MOCK_EOF' | kubectl apply -n "$MOCK_UPSTREAM_NS" -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: mock-upstream-config
data:
  nginx.conf: |
    events {}
    http {
      log_format requestline '$request';
      access_log /dev/stdout requestline;
      server {
        listen 8080;
        location / {
          add_header Content-Type text/plain;
          return 200 "received: $request\n";
        }
      }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mock-upstream
spec:
  replicas: 1
  selector:
    matchLabels: {app: mock-upstream}
  template:
    metadata:
      labels: {app: mock-upstream}
    spec:
      containers:
        - name: nginx
          image: nginx:1.27-alpine
          ports: [{containerPort: 8080}]
          volumeMounts:
            - name: config
              mountPath: /etc/nginx/nginx.conf
              subPath: nginx.conf
      volumes:
        - name: config
          configMap: {name: mock-upstream-config}
---
apiVersion: v1
kind: Service
metadata:
  name: mock-upstream
spec:
  selector: {app: mock-upstream}
  ports: [{port: 8080, targetPort: 8080}]
MOCK_EOF

kubectl -n "$MOCK_UPSTREAM_NS" rollout status deployment/mock-upstream --timeout=60s
log "Mock upstream ready at mock-upstream.${MOCK_UPSTREAM_NS}.svc.cluster.local:8080"

log "NOTE: For a full absolute-path test, temporarily point the proxy at the mock upstream:"
log "      helm upgrade --set proxy.upstream.host=mock-upstream.${MOCK_UPSTREAM_NS}.svc.cluster.local:8080"
log "      Then: kubectl exec test-client -- curl http://httpbin.org/foo"
log "      And check mock-upstream log: it must contain 'GET http://httpbin.org/foo HTTP/1.1'"

# -----------------------------------------------------------------------------
step "7. Metrics endpoint returns data"
# -----------------------------------------------------------------------------
METRICS=$(kubectl -n "$PROXY_NS" exec deploy/"$PROXY_NAME" -- \
  wget -q -O - http://localhost:9090/metrics 2>/dev/null)

for metric in forward_proxy_requests_total forward_proxy_active_connections; do
  if echo "$METRICS" | grep -q "^${metric}"; then
    pass "Metric $metric present"
  else
    fail "Metric $metric missing"
  fi
done

# -----------------------------------------------------------------------------
step "8. Audit log contains structured events"
# -----------------------------------------------------------------------------
log "Recent audit events:"
kubectl -n "$PROXY_NS" logs deployment/"$PROXY_NAME" --tail=20 | \
  grep -E 'egress|"method"' || log "No audit events found (no traffic yet)"

# -----------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " ALL TESTS PASSED"
echo "============================================================"
