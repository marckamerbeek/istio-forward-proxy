#!/usr/bin/env bash
# Build de istio-forward-proxy Docker image.
#
# Environment variabelen:
#   IMAGE_REPO   - registry + repo (default: localhost/istio-forward-proxy)
#   IMAGE_TAG    - tag (default: git commit short hash, fallback: dev)
#   PLATFORMS    - docker buildx platforms (default: linux/amd64)
#   PUSH         - set PUSH=1 om meteen te pushen met buildx
#
# Gebruik:
#   ./scripts/build.sh
#   IMAGE_TAG=v0.1.0 PUSH=1 ./scripts/build.sh
#   IMAGE_REPO=registry.corp/platform/istio-forward-proxy PUSH=1 ./scripts/build.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

IMAGE_REPO="${IMAGE_REPO:-localhost/istio-forward-proxy}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
IMAGE_TAG="${IMAGE_TAG:-$COMMIT}"
PLATFORMS="${PLATFORMS:-linux/amd64}"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
VERSION="${VERSION:-$IMAGE_TAG}"

echo ">>> Building ${IMAGE_REPO}:${IMAGE_TAG}"
echo "    platforms: ${PLATFORMS}"
echo "    commit:    ${COMMIT}"
echo "    date:      ${BUILD_DATE}"

BUILD_ARGS=(
  --build-arg "VERSION=${VERSION}"
  --build-arg "COMMIT=${COMMIT}"
  --build-arg "BUILD_DATE=${BUILD_DATE}"
  --file deploy/docker/Dockerfile
  --tag "${IMAGE_REPO}:${IMAGE_TAG}"
  --tag "${IMAGE_REPO}:latest"
)

if [[ "${PUSH:-0}" == "1" ]]; then
  docker buildx build \
    --platform "${PLATFORMS}" \
    "${BUILD_ARGS[@]}" \
    --push \
    .
else
  # Lokale build (single-platform)
  docker build "${BUILD_ARGS[@]}" .
fi

echo ">>> Done: ${IMAGE_REPO}:${IMAGE_TAG}"
