#!/usr/bin/env bash
# Installeer de istio-forward-proxy op een cluster met Istio ambient.
#
# Vereisten:
#   - kubectl met cluster admin rechten
#   - helm 3.14+
#   - Istio ambient mode al geïnstalleerd (istiod, ztunnel, CNI)
#   - cert-manager geïnstalleerd (voor mTLS client certs)
#   - De forward-proxy Docker image gepusht naar een registry die de
#     cluster kan bereiken

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

PROXY_NS="${PROXY_NS:-istio-egress}"
RELEASE_NAME="${RELEASE_NAME:-istio-forward-proxy}"
VALUES_FILE="${VALUES_FILE:-$ROOT/deploy/helm/istio-forward-proxy/values.yaml}"

log() { echo -e "\033[1;34m[INSTALL]\033[0m $*"; }

log "Check prereqs"
command -v kubectl >/dev/null || { echo "kubectl ontbreekt"; exit 1; }
command -v helm    >/dev/null || { echo "helm ontbreekt"; exit 1; }

log "Check Istio ambient"
if ! kubectl get crd ztunnels.install.istio.io >/dev/null 2>&1 && \
   ! kubectl -n istio-system get daemonset ztunnel >/dev/null 2>&1; then
  echo "WAARSCHUWING: ztunnel daemonset niet gevonden. Is Istio ambient geïnstalleerd?"
fi

log "Check cert-manager"
if ! kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
  echo "WAARSCHUWING: cert-manager CRDs niet gevonden."
  echo "   Of installeer cert-manager, of zet proxy.mtls.certManager.enabled=false"
fi

log "Maak namespace met ambient label"
kubectl apply -f "$ROOT/deploy/istio/00-namespace.yaml"

log "Helm install/upgrade"
helm upgrade --install "$RELEASE_NAME" \
  "$ROOT/deploy/helm/istio-forward-proxy" \
  --namespace "$PROXY_NS" \
  --values "$VALUES_FILE" \
  --wait \
  --timeout=5m \
  "$@"

log "Wacht op rollout"
kubectl -n "$PROXY_NS" rollout status deployment/"$RELEASE_NAME" --timeout=180s

log "Verificatie"
kubectl -n "$PROXY_NS" get pods,svc,certificate,peerauthentication,authorizationpolicy 2>/dev/null || \
  kubectl -n "$PROXY_NS" get pods,svc

log "Klaar. Zie NOTES voor gebruik."
