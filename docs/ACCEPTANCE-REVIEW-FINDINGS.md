# Acceptance review findings (2026-09-05)

Tracks the 13 findings from the external acceptance review of `istio-forward-proxy`
(version `0.1.0-8975d1a`) against Squid, Gloo Edge, and agentgateway:
https://github.com/kjumin/forward-proxy-acceptance-review

No source code was changed during that review — findings only, backed by
`results/` and `docs/acceptatierapport.html` in that repository. Check items off
here as they're fixed, and note the fix commit.

## High

- [ ] **F1 — Audit identity is forgeable by the client.** In ambient mode `r.TLS` is
      always `nil` (ztunnel terminates mTLS at L4), so the proxy always falls back to
      the client-supplied `X-Forwarded-Client-Cert` header for the SPIFFE identity in
      the audit record. A client can claim any identity, e.g.
      `spiffe://cluster.local/ns/kube-system/sa/cluster-admin`, and it's logged as fact.
      _Source: `message_integrity_test.py::audit-identity-not-forgeable`_
- [ ] **F2 — Chunked request bodies vanish, and their bytes are fed to the next hop as
      a request.** A `Transfer-Encoding: chunked` POST is forwarded with neither
      `Transfer-Encoding` nor `Content-Length` — under RFC 9112 §6 that combination
      means "no body". The next hop treats the request as complete and parses the
      still-arriving body bytes as the start of a new request: a request-smuggling
      primitive. Reproduced 5x; failure mode scales with body size (≤4MiB: silently
      dropped; 5-16MiB: 502; ≥32MiB: connection reset).
      _Source: `message_integrity_test.py::chunked-request-body-preserved`,
      `results/evidence/ifp-framing-recorder.txt`,
      `results/evidence/ifp-parent-400-during-256mib-body.txt`_
- [ ] **F9 — `--idle-timeout` is an absolute deadline from connection start, not an
      idle timeout.** `handleHTTPForward` calls `upstream.SetDeadline(...)` exactly
      once at request start and never resets it on activity. A live, slow-but-active
      transfer gets cut off at exactly the configured timeout, with no error status —
      the client just receives fewer bytes than `Content-Length` promised.
- [ ] **F10 — CONNECT tunnels have no timeout at all.** `handleConnect`/`tunnel()` set
      no deadline anywhere, so an idle tunnel is never reclaimed regardless of
      `--idle-timeout`. 25 fully-idle tunnels were still open at 2x the configured
      timeout. Slow-loris-style resource exhaustion vector; also pins a fd/goroutine
      forever on any client that disappears without a clean close.

## Medium

- [ ] **F11 — No upstream connection pooling.** No `http.Transport`/`http.Client` reuse
      anywhere in the codebase; confirmed at the parent (12 requests → 12 distinct
      upstream connections, ratio 1.00). Costs +72% latency on plain HTTP (1.8ms →
      3.1ms vs. Squid) and +115% with upstream mTLS enabled, since the TLS handshake
      is paid per request instead of once per connection. This is the direct cause of
      ifp being the slowest of the four measured proxies.
      _Source: `selfbuild_hazards_test.py::upstream-conn-churn` (run B),
      `performance_test.py::connection-reuse-profile`_
- [ ] **F3 — A failing upstream gives the client nothing, and the audit log says 503.**
      Against a reachable, ACL-allowed host with nothing listening, the proxy closes
      the client connection with no response at all, while the audit record for that
      same request claims `"status":503`.
- [ ] **F4 — `bytes_out` is always 0 in the per-request audit record**, including on
      successful 200s with a body; `bytes_in` carries the response size instead. The
      two fields look swapped. (Aggregated Prometheus counters are correct — this is
      specific to the per-request record.)
- [ ] **F5 — The audit record logs the raw client string, not the matched/normalized
      host.** ACL matching normalizes case and trailing dots correctly, but the audit
      log doesn't — so a SIEM rule doing exact-match on destination host silently
      misses traffic to the same host under a different spelling, entirely
      client-controlled.
- [ ] **F13 — The `Expect: 100-continue` expectation is answered locally instead of
      being relayed to the origin.** The origin never gets a chance to reject (401/
      413/403) before the body is sent — the entire point of 100-continue.
      _Source: `selfbuild_hazards_test.py::expect-100-continue-relayed`, run A_

## Low

- [ ] **F6 — `:9090/metrics` is unauthenticated and reachable cluster-wide.** No
      NetworkPolicy restricts it. Severity kept low because labels are aggregated only
      (`method`, `decision`, `direction`) with no destination hosts.
- [ ] **F7 — Nothing ties `mtls.enabled` to the upstream's actual scheme.** A plain-port
      upstream combined with the chart default `proxy.mtls.enabled: true` installs and
      reports `Healthy`, then every request 502s at runtime
      (`tls: first record does not look like a TLS handshake`). Should fail at Helm
      template/install time instead.
- [ ] **F8 — Default `RollingUpdate` strategy stalls with `replicaCount: 1`** on a node
      without headroom for two pods: new pod stays `Pending` (`Insufficient memory`),
      old pod never reclaimed. A single-replica component should default to
      `strategy: Recreate`.
- [ ] **F12 — `Proxy-Connection` (hop-by-hop) is relayed to the parent.** Correctly
      stripped toward the origin, but not toward the parent. Low severity: legacy,
      non-standardized header, no credential (contrast: agentgateway fails the same
      check by leaking `Proxy-Authorization`, a credential).

## Notes on reading the source review

- Three rows in the review's result matrix look favorable for ifp but aren't:
  `upstream-pool-not-shared` PASS (only because there's no pool at all — this is F11,
  the check itself was retracted), `large-chunked-body-streamed` PASS in run B (only
  because the body gets dropped — this is F2), and `idle-tunnel-deadline-enforced`
  PASS (something other than the proxy's own timeout closed the tunnel — recorded as
  undecided, not a pass).
- The ACL (`ServiceEntry`-based allowlist) held up against every normalization and
  address-spelling variant tried — 0 bypasses, the strongest part of the product.
