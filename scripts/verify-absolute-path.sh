#!/usr/bin/env bash
# Verify that the forward proxy sends ABSOLUTE paths to the upstream.
#
# This is the definitive proof that this proxy behaves differently from Envoy.
# A mock upstream logs raw request-lines; we send a request through the proxy
# and verify the log contains:
#   "GET http://target.example/path HTTP/1.1"   (absolute — correct)
# and NOT:
#   "GET /path HTTP/1.1"                         (relative — what Envoy would send)
#
# Prerequisites: cluster with Istio ambient + the proxy installed.

set -euo pipefail

PROXY_NS="${PROXY_NS:-istio-egress}"
PROXY_NAME="${PROXY_NAME:-istio-forward-proxy}"
TEST_NS="${TEST_NS:-forward-proxy-absolute-test}"

pass() { echo -e "\033[1;32m[PASS]\033[0m $*"; }
fail() { echo -e "\033[1;31m[FAIL]\033[0m $*"; exit 1; }
log()  { echo -e "\033[1;34m[TEST]\033[0m $*"; }

cleanup() { kubectl delete ns "$TEST_NS" --ignore-not-found --wait=false; }
trap cleanup EXIT

# -----------------------------------------------------------------------------
log "Setup test namespace and mock upstream"
# -----------------------------------------------------------------------------
kubectl create ns "$TEST_NS" --dry-run=client -o yaml | \
  kubectl label --local -f - istio.io/dataplane-mode=ambient -o yaml | \
  kubectl apply -f -

cat <<'EOF' | kubectl apply -n "$TEST_NS" -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: raw-logger-script
data:
  server.py: |
    import http.server, socketserver, sys
    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            sys.stdout.write(f"REQLINE: {self.requestline}\n")
            sys.stdout.flush()
            self.send_response(200)
            self.send_header("Content-Type","text/plain")
            self.end_headers()
            self.wfile.write(f"ok: {self.requestline}\n".encode())
        def log_message(self,*a,**k): pass
    with socketserver.TCPServer(("0.0.0.0",8080),H) as httpd:
        httpd.serve_forever()
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: raw-logger
spec:
  replicas: 1
  selector: {matchLabels: {app: raw-logger}}
  template:
    metadata: {labels: {app: raw-logger}}
    spec:
      containers:
        - name: py
          image: python:3.12-alpine
          command: ["python3", "/script/server.py"]
          ports: [{containerPort: 8080}]
          volumeMounts:
            - name: script
              mountPath: /script
      volumes:
        - name: script
          configMap: {name: raw-logger-script}
---
apiVersion: v1
kind: Service
metadata:
  name: raw-logger
spec:
  selector: {app: raw-logger}
  ports: [{port: 8080, targetPort: 8080}]
---
# Register raw-logger as an "external host" via ServiceEntry so the ACL allows it
apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: test-target
spec:
  hosts:
    - test-target.example
  ports:
    - number: 80
      name: http
      protocol: HTTP
  resolution: STATIC
  endpoints:
    - address: raw-logger.forward-proxy-absolute-test.svc.cluster.local
EOF

kubectl -n "$TEST_NS" rollout status deployment/raw-logger --timeout=60s
pass "Mock upstream 'raw-logger' ready"

# -----------------------------------------------------------------------------
log "Reconfigure proxy to point at raw-logger as upstream"
# -----------------------------------------------------------------------------
log "This requires manually updating the proxy Helm values:"
log "      helm upgrade $PROXY_NAME ./deploy/helm/istio-forward-proxy \\"
log "        --set proxy.upstream.host=raw-logger.${TEST_NS}.svc.cluster.local:8080 \\"
log "        --set proxy.mtls.enabled=false"
log ""
log "Done? (press Enter to continue, Ctrl-C to abort)"
read -r

# -----------------------------------------------------------------------------
log "Deploy test client and send a unique request"
# -----------------------------------------------------------------------------
UNIQUE_PATH="/test-marker-$(date +%s)"
PROXY_URL="http://${PROXY_NAME}.${PROXY_NS}.svc.cluster.local:3128"

kubectl -n "$TEST_NS" run curl-client --rm -i --restart=Never \
  --image=curlimages/curl:8.7.1 \
  --env="HTTP_PROXY=$PROXY_URL" \
  -- curl -sS "http://test-target.example${UNIQUE_PATH}" || true

# -----------------------------------------------------------------------------
log "Check raw-logger log for ABSOLUTE path"
# -----------------------------------------------------------------------------
sleep 2
LOG=$(kubectl -n "$TEST_NS" logs deployment/raw-logger)
echo "--- raw-logger output ---"
echo "$LOG"
echo "--- end ---"

EXPECTED_ABS="GET http://test-target.example${UNIQUE_PATH} HTTP/1.1"
UNEXPECTED_REL="GET ${UNIQUE_PATH} HTTP/1.1"

if echo "$LOG" | grep -qF "REQLINE: $EXPECTED_ABS"; then
  pass "ABSOLUTE path confirmed: '$EXPECTED_ABS'"
  pass "Proxy behaves correctly (unlike Envoy)"
elif echo "$LOG" | grep -qF "REQLINE: $UNEXPECTED_REL"; then
  fail "RELATIVE path received: '$UNEXPECTED_REL' — this is Envoy behavior"
else
  fail "No match found. Expected: '$EXPECTED_ABS'"
fi
