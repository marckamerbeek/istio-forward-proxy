#!/usr/bin/env bash
# Performance test for istio-forward-proxy's HTTP forward path.
#
# Starts:
#   1. A mock upstream (traefik/whoami) standing in for the real Squid/HTTP
#      upstream proxy
#   2. The forward-proxy binary with TLS disabled (--mtls=false)
#   3. A k6 load test that sends traffic through the proxy via HTTP_PROXY
#
# Prerequisites:
#   - A kube context pointing at a cluster with the Istio ServiceEntry CRD
#     registered (see .github/workflows/perf-test.yml for how CI sets this
#     up with kind + the istio/base Helm chart; locally this can be any
#     cluster with Istio or just the base CRDs installed)
#   - go, docker, kubectl, k6 on PATH
#
# Env overrides: VUS, DURATION (passed to k6), SUMMARY_EXPORT (k6 JSON
# summary output path), SKIP_CLEANUP=1 (keep the temp dir for debugging).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

TEST_HOST="perf-test.internal"
UPSTREAM_PORT=18080
PROXY_PORT=13128
METRICS_PORT=19090
WHOAMI_IMAGE="traefik/whoami:v1.12.0"
WHOAMI_CONTAINER="forward-proxy-perf-whoami"

TMPDIR="$(mktemp -d)"
SUMMARY_EXPORT="${SUMMARY_EXPORT:-$TMPDIR/summary.json}"

log()  { echo -e "\033[1;34m[PERF]\033[0m $*"; }
pass() { echo -e "\033[1;32m[PASS]\033[0m $*"; }
fail() { echo -e "\033[1;31m[FAIL]\033[0m $*"; exit 1; }
step() { echo -e "\n\033[1;33m>>> $*\033[0m"; }

cleanup() {
  log "Cleanup..."
  kill "${PROXY_PID:-}" 2>/dev/null || true
  docker rm -f "$WHOAMI_CONTAINER" >/dev/null 2>&1 || true
  if [[ "${SKIP_CLEANUP:-0}" != "1" ]]; then
    rm -rf "$TMPDIR"
  else
    log "SKIP_CLEANUP=1, keeping $TMPDIR"
  fi
}
trap cleanup EXIT

for bin in go docker kubectl k6; do
  command -v "$bin" >/dev/null || fail "$bin is required but not on PATH"
done

# -----------------------------------------------------------------------------
step "1. Wait for the ServiceEntry CRD and register the test host"
# -----------------------------------------------------------------------------
kubectl wait --for=condition=Established crd/serviceentries.networking.istio.io --timeout=60s
pass "ServiceEntry CRD established"

cat <<EOF | kubectl apply -f -
apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: perf-test-host
  namespace: default
spec:
  hosts:
    - $TEST_HOST
  ports:
    - number: 80
      name: http
      protocol: HTTP
  resolution: DNS
  location: MESH_EXTERNAL
EOF
pass "ServiceEntry allowing $TEST_HOST applied"

# -----------------------------------------------------------------------------
step "2. Start mock upstream ($WHOAMI_IMAGE) on :$UPSTREAM_PORT"
# -----------------------------------------------------------------------------
docker rm -f "$WHOAMI_CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$WHOAMI_CONTAINER" -p "$UPSTREAM_PORT:80" "$WHOAMI_IMAGE" >/dev/null

for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:$UPSTREAM_PORT/" >/dev/null; then
    pass "Mock upstream is responding"
    break
  fi
  sleep 1
done
curl -sf "http://127.0.0.1:$UPSTREAM_PORT/" >/dev/null || fail "Mock upstream did not become ready"

# -----------------------------------------------------------------------------
step "3. Build and start forward-proxy on :$PROXY_PORT (mTLS disabled)"
# -----------------------------------------------------------------------------
go build -o "$TMPDIR/forward-proxy" ./cmd
pass "Binary built: $TMPDIR/forward-proxy"

"$TMPDIR/forward-proxy" \
  --listen=":$PROXY_PORT" \
  --metrics=":$METRICS_PORT" \
  --upstream="127.0.0.1:$UPSTREAM_PORT" \
  --mtls=false \
  --log-level=info \
  > "$TMPDIR/proxy.log" 2>&1 &
PROXY_PID=$!

for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:$METRICS_PORT/readyz" >/dev/null; then
    pass "forward-proxy is ready (ServiceEntry cache synced)"
    break
  fi
  sleep 1
done
curl -sf "http://127.0.0.1:$METRICS_PORT/readyz" >/dev/null || {
  log "forward-proxy did not become ready. Log:"
  cat "$TMPDIR/proxy.log"
  fail "forward-proxy readiness check failed"
}

curl -sf "http://127.0.0.1:$METRICS_PORT/debug/allowlist" | grep -q "$TEST_HOST" \
  || fail "$TEST_HOST not present in allowlist — ServiceEntry did not sync as expected"
pass "$TEST_HOST present in allowlist"

# -----------------------------------------------------------------------------
step "4. Run k6 load test through the proxy"
# -----------------------------------------------------------------------------
log "VUS=${VUS:-20} DURATION=${DURATION:-30s} target=http://$TEST_HOST/"

set +e
HTTP_PROXY="http://127.0.0.1:$PROXY_PORT" \
TARGET_URL="http://$TEST_HOST/" \
VUS="${VUS:-20}" \
DURATION="${DURATION:-30s}" \
  k6 run --summary-export="$SUMMARY_EXPORT" scripts/perf/k6-load-test.js
K6_STATUS=$?
set -e

if [[ $K6_STATUS -ne 0 ]]; then
  log "k6 run failed or thresholds breached. forward-proxy log tail:"
  tail -40 "$TMPDIR/proxy.log"
  fail "Performance test failed (k6 exit code $K6_STATUS)"
fi

pass "Performance test passed. Summary: $SUMMARY_EXPORT"
