# syntax=docker/dockerfile:1.7
# --------------------------------------------------------------------
# Build stage: compile a static Go binary for use with distroless.
# --------------------------------------------------------------------
FROM golang:1.22-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Build static binary with version info
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /out/forward-proxy ./cmd

# --------------------------------------------------------------------
# Runtime stage: distroless static, non-root user
# --------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/forward-proxy /usr/local/bin/forward-proxy

# Non-root (UID 65532 is 'nonroot' in distroless)
USER 65532:65532

EXPOSE 3128 9090

ENTRYPOINT ["/usr/local/bin/forward-proxy"]
