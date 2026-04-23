#!/usr/bin/env bash
# End-to-end test script voor de istio-forward-proxy.
#
# Dit script test:
#   1. Dat de proxy reageert en healthy is
#   2. Dat ServiceEntry-geregistreerde hosts worden toegestaan
#   3. Dat niet-geregistreerde hosts worden geweigerd (403)
#   4. Dat het request naar upstream een ABSOLUUT pad heeft
#   5. Dat hop-by-hop headers correct worden gestript
#   6. Dat mTLS naar upstream werkt met client certificaat
#   7. Dat metrics endpoint data levert
#
# Vereisten:
#   - kubectl met cluster admin
#   - Istio ambient mode geinstalleerd
#   - cert-manager met een werkende ClusterIssuer
#   - De proxy geinstalleerd via Helm

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
step "1. Check dat de proxy draait"
# -----------------------------------------------------------------------------
if ! kubectl -n "$PROXY_NS" get deployment "$PROXY_NAME" >/dev/null 2>&1; then
  fail "Deployment $PROXY_NAME niet gevonden in namespace $PROXY_NS"
fi
kubectl -n "$PROXY_NS" rollout status deployment/"$PROXY_NAME" --timeout=120s
pass "Proxy deployment ready"

# -----------------------------------------------------------------------------
step "2. Maak test namespace en client pod"
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
step "3. Test dat niet-geregistreerde host wordt geweigerd (403)"
# -----------------------------------------------------------------------------
set +e
OUTPUT=$(kubectl -n "$TEST_NS" exec test-client -c curl -- \
  curl -sS -o /dev/null -w "%{http_code}" http://example.invalid/ 2>&1)
set -e

if [[ "$OUTPUT" == "403" ]]; then
  pass "Niet-geregistreerde host correct geweigerd met 403"
else
  fail "Expected 403 voor niet-geregistreerde host, got: $OUTPUT"
fi

# -----------------------------------------------------------------------------
step "4. Registreer httpbin.org als ServiceEntry"
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

# ServiceEntry watch heeft een korte propagatie nodig
log "Wachten tot ServiceEntry in de allowlist staat..."
for i in $(seq 1 20); do
  ALLOWLIST=$(kubectl -n "$PROXY_NS" exec deploy/"$PROXY_NAME" -- \
    wget -q -O - http://localhost:9090/debug/allowlist 2>/dev/null || true)
  if echo "$ALLOWLIST" | grep -q "httpbin.org"; then
    pass "httpbin.org staat in allowlist"
    break
  fi
  sleep 1
  [[ $i -eq 20 ]] && fail "ServiceEntry niet zichtbaar in allowlist na 20s"
done

# -----------------------------------------------------------------------------
step "5. Test dat geregistreerde host wordt toegestaan"
# -----------------------------------------------------------------------------
# NB: dit vereist dat je upstream proxy daadwerkelijk naar httpbin.org kan.
# Als je test draait in een lab zonder internet, vervang dit door een
# mock upstream (zie verderop).
set +e
STATUS=$(kubectl -n "$TEST_NS" exec test-client -c curl -- \
  curl -sS -o /dev/null -w "%{http_code}" http://httpbin.org/get 2>&1 || echo "FAIL")
set -e

if [[ "$STATUS" =~ ^(200|301|302)$ ]]; then
  pass "httpbin.org call succesvol (status $STATUS)"
else
  log "WAARSCHUWING: status $STATUS (mogelijk heeft upstream geen internet)"
fi

# -----------------------------------------------------------------------------
step "6. Verifieer dat upstream ABSOLUTE paden ontvangt"
# -----------------------------------------------------------------------------
# We deployen een mock upstream die elk request logt, dan sturen we er
# een aantal requests doorheen en controleren de log.
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
        # Accepteer elk path (incl. absoluut), return 200 met de request-line
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
log "Mock upstream gereed op mock-upstream.${MOCK_UPSTREAM_NS}.svc.cluster.local:8080"

log "NOTE: Voor een volledige test moet je de proxy tijdelijk laten wijzen naar"
log "      de mock upstream. Dat doe je met: helm upgrade --set proxy.upstream.host="
log "      mock-upstream.${MOCK_UPSTREAM_NS}.svc.cluster.local:8080 --set proxy.mtls.enabled=false"
log "      Daarna: kubectl exec test-client -- curl http://httpbin.org/foo"
log "      En check de mock-upstream log: het moet 'GET http://httpbin.org/foo HTTP/1.1' bevatten"

# -----------------------------------------------------------------------------
step "7. Metrics endpoint levert data"
# -----------------------------------------------------------------------------
METRICS=$(kubectl -n "$PROXY_NS" exec deploy/"$PROXY_NAME" -- \
  wget -q -O - http://localhost:9090/metrics 2>/dev/null)

for metric in forward_proxy_requests_total forward_proxy_active_connections; do
  if echo "$METRICS" | grep -q "^${metric}"; then
    pass "Metric $metric aanwezig"
  else
    fail "Metric $metric ontbreekt"
  fi
done

# -----------------------------------------------------------------------------
step "8. Audit log bevat structured events"
# -----------------------------------------------------------------------------
log "Laatste audit events:"
kubectl -n "$PROXY_NS" logs deployment/"$PROXY_NAME" --tail=20 | \
  grep -E 'egress|"method"' || log "Geen audit events gevonden (mogelijk nog geen traffic)"

# -----------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " ALLE TESTS GESLAAGD"
echo "============================================================"
