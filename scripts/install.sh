#!/usr/bin/env bash
# Install istio-forward-proxy on a cluster with Istio ambient mode.
#
# Prerequisites:
#   - kubectl with cluster admin rights
#   - helm 3.14+
#   - Istio ambient mode installed (istiod, ztunnel, CNI)
#   - cert-manager installed (for mTLS client certs)
#   - The forward-proxy Docker image pushed to a registry reachable by the cluster

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

PROXY_NS="${PROXY_NS:-istio-egress}"
RELEASE_NAME="${RELEASE_NAME:-istio-forward-proxy}"
VALUES_FILE="${VALUES_FILE:-$ROOT/deploy/helm/istio-forward-proxy/values.yaml}"

log() { echo -e "\033[1;34m[INSTALL]\033[0m $*"; }

log "Checking prerequisites"
command -v kubectl >/dev/null || { echo "kubectl not found"; exit 1; }
command -v helm    >/dev/null || { echo "helm not found"; exit 1; }

log "Checking Istio ambient"
if ! kubectl get crd ztunnels.install.istio.io >/dev/null 2>&1 && \
   ! kubectl -n istio-system get daemonset ztunnel >/dev/null 2>&1; then
  echo "WARNING: ztunnel daemonset not found. Is Istio ambient mode installed?"
fi

log "Checking cert-manager"
if ! kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
  echo "WARNING: cert-manager CRDs not found."
  echo "   Either install cert-manager, or set proxy.mtls.certManager.enabled=false"
fi

log "Creating namespace with ambient label"
kubectl apply -f "$ROOT/deploy/istio/00-namespace.yaml"

log "Running helm install/upgrade"
helm upgrade --install "$RELEASE_NAME" \
  "$ROOT/deploy/helm/istio-forward-proxy" \
  --namespace "$PROXY_NS" \
  --values "$VALUES_FILE" \
  --wait \
  --timeout=5m \
  "$@"

log "Waiting for rollout"
kubectl -n "$PROXY_NS" rollout status deployment/"$RELEASE_NAME" --timeout=180s

log "Verification"
kubectl -n "$PROXY_NS" get pods,svc,certificate,peerauthentication,authorizationpolicy 2>/dev/null || \
  kubectl -n "$PROXY_NS" get pods,svc

log "Done. See NOTES for usage."
