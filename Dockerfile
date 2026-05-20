# syntax=docker/dockerfile:1.7
# --------------------------------------------------------------------
# Build stage: compile a static Go binary for use with distroless.
# --------------------------------------------------------------------
FROM golang:1.26-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE

WORKDIR /src

# 1. Kopieer EERST alle bestanden (inclusief go.mod, cmd, en internal)
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# 2. Voer DAARNA pas de download uit, zodat Go de interne pakketten direct ziet
RUN go mod download

# 3. Bouw de static binary met de versie info
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /out/forward-proxy ./cmd

# --------------------------------------------------------------------
# Runtime stage: distroless static, non-root user (blijft hetzelfde)
# --------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/forward-proxy /usr/local/bin/forward-proxy

USER 65532:65532

EXPOSE 3128 9090

ENTRYPOINT ["/usr/local/bin/forward-proxy"]
