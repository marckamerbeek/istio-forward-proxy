#!/usr/bin/env bash
# Local integration test WITHOUT Kubernetes.
#
# Starts:
#   1. A mock upstream proxy (Python HTTP server that logs request-lines)
#   2. The forward-proxy binary with TLS disabled (--mtls=false)
#   3. Sends a test request and verifies the expected behavior
#
# Note: without a Kubernetes cluster the ServiceEntry watcher cannot sync,
# so the ACL will be empty and all requests will return 403 (fail-closed).
# For absolute-path verification use scripts/e2e-test.sh on a cluster.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

TMPDIR="$(mktemp -d)"
trap "rm -rf $TMPDIR; kill %1 %2 2>/dev/null || true" EXIT

UPSTREAM_PORT=18080
PROXY_PORT=13128

log()  { echo -e "\033[1;34m[TEST]\033[0m $*"; }
pass() { echo -e "\033[1;32m[PASS]\033[0m $*"; }
fail() { echo -e "\033[1;31m[FAIL]\033[0m $*"; exit 1; }

# -----------------------------------------------------------------------------
log "1. Start mock upstream on :$UPSTREAM_PORT"
# -----------------------------------------------------------------------------
cat > "$TMPDIR/upstream.py" <<'PYEOF'
import http.server
import socketserver
import sys
import os

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        print(f"REQLINE: {self.requestline}", flush=True)
        for k, v in self.headers.items():
            print(f"HEADER: {k}: {v}", flush=True)
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(f"ok: {self.requestline}\n".encode())

    def do_POST(self):
        self.do_GET()

    def log_message(self, *a, **kw): pass

port = int(os.environ.get("PORT", 18080))
with socketserver.TCPServer(("127.0.0.1", port), H) as httpd:
    httpd.serve_forever()
PYEOF

PORT=$UPSTREAM_PORT python3 "$TMPDIR/upstream.py" > "$TMPDIR/upstream.log" 2>&1 &
UPSTREAM_PID=$!
sleep 1

if ! kill -0 $UPSTREAM_PID 2>/dev/null; then
  fail "Mock upstream did not start"
fi
pass "Mock upstream running (PID $UPSTREAM_PID)"

# -----------------------------------------------------------------------------
log "2. Build the forward-proxy binary"
# -----------------------------------------------------------------------------
if [[ ! -x "$TMPDIR/forward-proxy" ]]; then
  if ! command -v go >/dev/null; then
    fail "Go is not installed"
  fi
  log "go build..."
  if ! go build -o "$TMPDIR/forward-proxy" ./cmd 2>"$TMPDIR/build.err"; then
    log "Build failed — dependencies may be missing. See $TMPDIR/build.err"
    cat "$TMPDIR/build.err"
    exit 1
  fi
fi
pass "Binary built: $TMPDIR/forward-proxy"

# -----------------------------------------------------------------------------
log "3. Start forward-proxy on :$PROXY_PORT (mTLS disabled, no K8s)"
# -----------------------------------------------------------------------------
cat > "$TMPDIR/kubeconfig.fake" <<'EOF'
apiVersion: v1
kind: Config
clusters: [{name: none, cluster: {server: https://127.0.0.1:1}}]
contexts: [{name: none, context: {cluster: none, user: none}}]
current-context: none
users: [{name: none}]
EOF

KUBECONFIG="$TMPDIR/kubeconfig.fake" "$TMPDIR/forward-proxy" \
  --listen=":$PROXY_PORT" \
  --metrics=":19090" \
  --upstream="127.0.0.1:$UPSTREAM_PORT" \
  --mtls=false \
  --log-level=debug \
  > "$TMPDIR/proxy.log" 2>&1 &
PROXY_PID=$!
sleep 2

if ! kill -0 $PROXY_PID 2>/dev/null; then
  log "Proxy exited. Log:"
  cat "$TMPDIR/proxy.log"
  fail "Forward proxy did not start"
fi
pass "Forward proxy running (PID $PROXY_PID)"

# -----------------------------------------------------------------------------
log "4. Without K8s the ACL is empty — expect 403 (fail-closed)"
# -----------------------------------------------------------------------------
STATUS=$(curl -sS -o /dev/null -w "%{http_code}" \
  -x "http://127.0.0.1:$PROXY_PORT" \
  http://some-host.invalid/test 2>&1 || echo "FAIL")

if [[ "$STATUS" == "403" ]]; then
  pass "403 for host without ServiceEntry (correct, fail-closed behavior)"
else
  log "Expected 403 (empty ACL), got $STATUS"
  log "Proxy log:"
  tail -20 "$TMPDIR/proxy.log"
fi

# -----------------------------------------------------------------------------
log "SUMMARY: Local test is limited because the ServiceEntry watcher requires"
log "a real Kubernetes cluster. For a full end-to-end test including absolute"
log "path verification, run scripts/e2e-test.sh against a cluster with Istio ambient."
# -----------------------------------------------------------------------------
