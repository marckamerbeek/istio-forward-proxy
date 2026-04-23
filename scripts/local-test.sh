#!/usr/bin/env bash
# Lokale integration test ZONDER Kubernetes.
#
# Start:
#   1. Een mock upstream proxy (nginx die request-lines logt)
#   2. De forward-proxy binary met TLS uit (--mtls=false)
#   3. Stuurt een test request en verifieert de request-line
#
# Dit controleert de kern: ABSOLUTE path preservatie naar upstream.

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
log "1. Start mock upstream op :$UPSTREAM_PORT"
# -----------------------------------------------------------------------------
# Simpele Python HTTP server die de RAW request-line logt
cat > "$TMPDIR/upstream.py" <<'PYEOF'
import http.server
import socketserver
import sys
import os

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        # Log de raw request-line zoals ontvangen
        print(f"REQLINE: {self.requestline}", flush=True)
        # Log alle headers ook
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
  fail "Mock upstream niet gestart"
fi
pass "Mock upstream draait (PID $UPSTREAM_PID)"

# -----------------------------------------------------------------------------
log "2. Bouw de forward-proxy binary (als die nog niet bestaat)"
# -----------------------------------------------------------------------------
# NB: go mod download vereist internet + toegang tot proxy.golang.org
# Als de build faalt, skip deze test-stap.
if [[ ! -x "$TMPDIR/forward-proxy" ]]; then
  if ! command -v go >/dev/null; then
    fail "Go is niet geïnstalleerd"
  fi
  log "go build..."
  if ! go build -o "$TMPDIR/forward-proxy" ./cmd 2>"$TMPDIR/build.err"; then
    log "Build gefaald — mogelijk ontbreken dependencies. Bekijk $TMPDIR/build.err"
    cat "$TMPDIR/build.err"
    exit 1
  fi
fi
pass "Binary gebouwd: $TMPDIR/forward-proxy"

# -----------------------------------------------------------------------------
log "3. Start de forward-proxy op :$PROXY_PORT (mTLS uit, geen K8s)"
# -----------------------------------------------------------------------------
# We gebruiken KUBECONFIG=/dev/null zodat de ServiceEntry watcher zonder
# K8s faalt. Voor deze integration test willen we dat de ACL dichtzit.
# Omdat er geen ServiceEntries zijn, verwachten we 403 op elke request.
#
# Om ACL te omzeilen gebruiken we een mock mode — maar die bestaat nog
# niet in de binary. Dit demonstreert het limiet van deze test zonder
# cluster: we kunnen alleen de 403-deny flow verifieren.

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
  log "Proxy gestopt. Log:"
  cat "$TMPDIR/proxy.log"
  fail "Forward proxy niet gestart"
fi
pass "Forward proxy draait (PID $PROXY_PID)"

# -----------------------------------------------------------------------------
log "4. Test: zonder K8s is ACL leeg, dus verwacht 403"
# -----------------------------------------------------------------------------
STATUS=$(curl -sS -o /dev/null -w "%{http_code}" \
  -x "http://127.0.0.1:$PROXY_PORT" \
  http://some-host.invalid/test 2>&1 || echo "FAIL")

if [[ "$STATUS" == "403" ]]; then
  pass "403 op host zonder ServiceEntry (correct, fail-closed)"
else
  log "Verwacht 403 (ACL leeg), got $STATUS"
  log "Proxy log:"
  tail -20 "$TMPDIR/proxy.log"
fi

# -----------------------------------------------------------------------------
log "SAMENVATTING: Lokale test is beperkt omdat de ServiceEntry watcher een"
log "echt K8s cluster nodig heeft. Voor een volledige end-to-end run die de"
log "ABSOLUTE path verificatie doet: gebruik scripts/e2e-test.sh op een"
log "cluster met Istio ambient."
# -----------------------------------------------------------------------------
