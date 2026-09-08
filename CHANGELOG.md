# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **Performance:** every plain-HTTP request used to dial a fresh TCP (and,
  with mTLS, TLS) connection to the upstream proxy — no connection pooling
  or reuse at all, measured in the external acceptance review at +72%
  latency per request, +115% with upstream mTLS enabled (finding F11,
  issue #29). The forward path now goes through a pooled `http.Transport`
  (`Transport.Proxy` writes requests to the upstream in absolute-form
  automatically, the same way any Go HTTP client behaves through an
  `HTTP_PROXY`), instead of a hand-written per-request dial and wire
  writer. CONNECT tunnels are unaffected — a tunnel already holds its
  connection for its own lifetime, so pooling doesn't apply there.
- `writeProxyRequest`, the hand-rolled request writer this replaces
  (including its own chunked-body re-framing added for F2, issue #26), is
  removed: `http.Transport` frames a request with an unknown-length body
  as `Transfer-Encoding: chunked` correctly by construction, so the
  hand-written equivalent is no longer needed.

## [0.2.0] - 2026-09-08

An external acceptance review of this proxy against Squid, Gloo Edge, and
agentgateway (see `docs/ACCEPTANCE-REVIEW-FINDINGS.md`) found several
correctness and security defects in request handling. This release fixes
the four most severe ones.

### Added

- `--trust-xfcc-header` / `TRUST_XFCC_HEADER` flag (`proxy.audit.trustXFCCHeader`
  in the Helm chart, default `false`): explicit opt-in to trust a
  client-supplied `X-Forwarded-Client-Cert` header for the audit log's
  SPIFFE identity when there is no verified mTLS peer certificate on the
  connection. See "Audit identity" in the README. (#40)
- CI: `go vet` and `go test` now run on every push and pull request to
  `master` (`.github/workflows/go-test.yml`). Previously only a Docker
  build and a k6 performance test ran in CI, neither of which compiles
  `_test.go` files, so a `_test.go`-only syntax error could land on
  `master` unnoticed. (#41)

### Fixed

- **Security:** the audit log's SPIFFE identity could be forged by any
  caller via a self-supplied `X-Forwarded-Client-Cert` header. In ambient
  mode there is never a verified mTLS peer certificate to check it
  against — ztunnel terminates HBONE mTLS at L4 and neither sets nor
  sanitizes that header — so the fallback was the only path taken in
  practice. The header is no longer trusted unless `TRUST_XFCC_HEADER` is
  explicitly enabled. (#40)
- **Security:** a request with `Transfer-Encoding: chunked` was forwarded
  to the upstream with neither `Transfer-Encoding` nor `Content-Length`,
  so the upstream read it as a zero-length body and parsed the
  still-arriving body bytes as the start of the next request on the same
  connection — a request-smuggling primitive. Chunked bodies are now
  correctly re-declared and re-chunked on the way upstream instead of
  being silently misframed. (#39)
- `--idle-timeout` was an absolute deadline counted from when the upstream
  connection was opened, not an actual idle timeout: it could cut off a
  slow-but-live HTTP response body partway through, with no error status,
  regardless of how much legitimate traffic was still flowing. It now
  resets on every read/write, so it only fires after a genuine gap with no
  activity. (#42)
- CONNECT tunnels had no timeout at all: an abandoned tunnel — opened,
  then never closed cleanly by the client — stayed open forever, pinning a
  goroutine and two file descriptors per tunnel. CONNECT tunnels now get
  the same idle-timeout treatment as the plain HTTP forward path. (#43)
- Fixed a build-breaking syntax error in `internal/proxy/handler_test.go`
  (a missing closing brace from an earlier merge) that had been invisible
  to CI until the `go vet`/`go test` workflow above was added. (#41)

### Documentation

- Corrected README.md and docs/ARCHITECTURE.md claims that ztunnel
  attaches or preserves the SPIFFE identity for this proxy to log — it
  does not; see the new "Audit identity" section in the README. (#40)
- Added `docs/ACCEPTANCE-REVIEW-FINDINGS.md`, a tracked checklist of all
  13 findings (F1–F13) from the external acceptance review, cross-linked
  to their GitHub issues. (#38)

## [0.1.7] - 2026-08-30

### Fixed

- **Security:** `/debug/allowlist` (on the `:9090` metrics/health/debug
  port) had no source restriction and dumped every team's
  ServiceEntry-derived ACL to any pod in the cluster. It now checks
  `r.RemoteAddr` and returns `403` unless the caller is loopback —
  matching every documented way to use it (`kubectl exec`,
  `kubectl port-forward`). `/metrics` and `/healthz`/`/readyz` are
  unaffected, since kubelet health probes have no identity a policy could
  otherwise scope to. (#21)

## [0.1.6] - 2026-08-30

### Changed

- Bumped base images.

## [0.1.5] - 2026-08-30

### Changed

- Bumped Dockerfile base image versions.

## [0.1.4] - 2026-08-30

### Changed

- Bumped Go toolchain to 1.27.0.

## [0.1.3] - 2026-08-07

### Fixed

- Security patches and dependency updates.

## [0.1.0] - 2026-08-06

Initial versioned release.
