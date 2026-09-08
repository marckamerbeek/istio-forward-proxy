// Package proxy implements the core forward proxy logic. Two request types
// are handled:
//
//  1. HTTP forward with absolute path preservation. The proxy receives a
//     request with an absolute URI (GET http://host/path HTTP/1.1), validates
//     the host against the ACL, opens an mTLS connection to the upstream
//     proxy, and forwards the request with the absolute URI intact.
//
//  2. CONNECT tunnel for HTTPS traffic. After an ACL check the proxy opens
//     an mTLS connection to the upstream, forwards the CONNECT, and tunnels
//     TCP bidirectionally after a 200 Connection Established response.
//
// The key difference from Envoy's TLS origination is that the absolute
// request-line is NOT rewritten to a relative path. Envoy does this per
// RFC 7230 §5.3.1 (for origin servers); this proxy keeps the absolute form
// because the upstream is a proxy (RFC 7230 §5.3.2).
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marckamerbeek/istio-forward-proxy/internal/audit"
	"github.com/marckamerbeek/istio-forward-proxy/internal/certs"
	"github.com/marckamerbeek/istio-forward-proxy/internal/serviceentry"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "forward_proxy_requests_total",
		Help: "Total proxy requests by method and decision",
	}, []string{"method", "decision"})

	activeConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "forward_proxy_active_connections",
		Help: "Currently active upstream connections",
	})

	upstreamDialErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "forward_proxy_upstream_dial_errors_total",
		Help: "Failed upstream dials",
	})

	bytesTransferred = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "forward_proxy_bytes_transferred_total",
		Help: "Bytes transferred through the proxy by direction",
	}, []string{"direction"})
)

// Handler implements http.Handler, dispatching to the appropriate flow
// based on the request method (CONNECT vs plain HTTP).
type Handler struct {
	UpstreamProxy      string // host:port of the upstream proxy
	UpstreamAuth       string // Proxy-Authorization header value
	CertManager        *certs.Manager
	ACL                *serviceentry.Watcher
	Audit              *audit.Logger
	ExtraHeaders       map[string]string
	DialTimeout        time.Duration
	IdleTimeout        time.Duration
	TLSEnabled         bool
	InsecureSkipVerify bool
	// TrustXFCCHeader allows a client-supplied X-Forwarded-Client-Cert header
	// to populate the audit log's SPIFFE identity when there is no verified
	// mTLS peer certificate on the connection. Off by default: nothing
	// between an ambient-mode client and this proxy verifies that header, so
	// trusting it would let a caller dictate its own audit identity. See
	// spiffeFromRequest.
	TrustXFCCHeader bool
	Logger          *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}
	h.handleHTTPForward(w, r)
}

// -----------------------------------------------------------------------------
// HTTP forward with absolute path preservation
// -----------------------------------------------------------------------------

func (h *Handler) handleHTTPForward(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// A forward proxy request must carry an absolute URI (RFC 7230 §5.3.2).
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "proxy request requires absolute URI", http.StatusBadRequest)
		requestsTotal.WithLabelValues("HTTP", "bad_request").Inc()
		return
	}

	targetHost, targetPort := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))

	allow, _ := h.ACL.AllowHost(targetHost, targetPort)
	if !allow {
		h.denyForward(w, r, targetHost, targetPort, "host_not_in_service_entry_allowlist")
		return
	}

	upstream, err := h.dialUpstream()
	if err != nil {
		upstreamDialErrors.Inc()
		h.Logger.Error("upstream dial failed", "error", err, "target_host", targetHost)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		requestsTotal.WithLabelValues("HTTP", "upstream_error").Inc()
		return
	}
	upstream = wrapIdleTimeout(upstream, h.IdleTimeout)
	defer upstream.Close()
	activeConnections.Inc()
	defer activeConnections.Dec()

	// Write request with ABSOLUTE URI intact. This is the core difference from
	// Envoy, which would write: GET /path HTTP/1.1
	// We write:                 GET http://host:port/path HTTP/1.1
	if err := h.writeProxyRequest(upstream, r); err != nil {
		h.Logger.Error("write upstream request failed", "error", err)
		http.Error(w, "upstream write failed", http.StatusBadGateway)
		requestsTotal.WithLabelValues("HTTP", "upstream_error").Inc()
		return
	}

	br := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(br, r)
	if err != nil {
		h.Logger.Error("read upstream response failed", "error", err)
		http.Error(w, "upstream read failed", http.StatusBadGateway)
		requestsTotal.WithLabelValues("HTTP", "upstream_error").Inc()
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	bytesTransferred.WithLabelValues("upstream_to_client").Add(float64(n))

	h.Audit.Log(audit.Event{
		Timestamp:     start,
		ClientAddr:    r.RemoteAddr,
		SPIFFE:        h.spiffeFromRequest(r),
		Method:        "HTTP-FORWARD",
		TargetHost:    targetHost,
		TargetPort:    targetPort,
		UpstreamProxy: h.UpstreamProxy,
		Decision:      "allow",
		Status:        resp.StatusCode,
		BytesIn:       n,
		DurationMS:    time.Since(start).Milliseconds(),
	})
	requestsTotal.WithLabelValues("HTTP", "allow").Inc()
}

// writeProxyRequest writes an HTTP/1.1 request to the upstream connection
// with the absolute URI intact.
func (h *Handler) writeProxyRequest(w io.Writer, r *http.Request) error {
	absURI := r.URL.String()

	if _, err := fmt.Fprintf(w, "%s %s HTTP/1.1\r\n", r.Method, absURI); err != nil {
		return err
	}

	if err := writeHeader(w, "Host", r.Host); err != nil {
		return err
	}
	if h.UpstreamAuth != "" {
		if err := writeHeader(w, "Proxy-Authorization", h.UpstreamAuth); err != nil {
			return err
		}
	}
	for name, value := range h.ExtraHeaders {
		if err := writeHeader(w, name, value); err != nil {
			return err
		}
	}

	for k, vv := range r.Header {
		if isHopByHop(k) || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Proxy-Authorization") {
			continue
		}
		for _, v := range vv {
			if err := writeHeader(w, k, v); err != nil {
				return err
			}
		}
	}

	hasBody := r.Body != nil && r.Body != http.NoBody

	// A request whose length is unknown (net/http sets ContentLength -1 for
	// this) had Transfer-Encoding: chunked on the wire from the client -- and
	// net/http already stripped that header out of r.Header above, consuming
	// it into r.TransferEncoding instead. Without re-declaring some framing
	// here, the body below would go out with neither Content-Length nor
	// Transfer-Encoding: the upstream reads a zero-length body, considers the
	// request complete, and parses the body bytes that follow as the start of
	// the next request on the same connection. Re-chunk instead of buffering
	// to compute Content-Length, so streaming a body larger than memory still
	// works.
	chunked := hasBody && r.ContentLength < 0
	if chunked {
		if err := writeHeader(w, "Transfer-Encoding", "chunked"); err != nil {
			return err
		}
	}

	if _, err := w.Write([]byte("\r\n")); err != nil {
		return err
	}

	if hasBody {
		var n int64
		var err error
		if chunked {
			cw := httputil.NewChunkedWriter(w)
			if n, err = io.Copy(cw, r.Body); err == nil {
				if err = cw.Close(); err == nil {
					// cw.Close() only emits the "0\r\n" last-chunk line; the
					// empty trailer section still needs its own terminating
					// CRLF (RFC 9112 §7.1.3) or the upstream blocks waiting
					// for a trailer field that never arrives.
					_, err = w.Write([]byte("\r\n"))
				}
			}
		} else {
			n, err = io.Copy(w, r.Body)
		}
		if err != nil {
			return err
		}
		bytesTransferred.WithLabelValues("client_to_upstream").Add(float64(n))
	}
	return nil
}

// -----------------------------------------------------------------------------
// CONNECT tunnel for HTTPS traffic
// -----------------------------------------------------------------------------

func (h *Handler) handleConnect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	targetHost, targetPort := splitHostPort(r.Host, 443)

	allow, _ := h.ACL.AllowHost(targetHost, targetPort)
	if !allow {
		h.denyConnect(w, r, targetHost, targetPort, "host_not_in_service_entry_allowlist")
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		h.Logger.Error("hijack failed", "error", err)
		return
	}
	defer clientConn.Close()

	upstream, err := h.dialUpstream()
	if err != nil {
		upstreamDialErrors.Inc()
		h.Logger.Error("upstream dial failed", "error", err, "target_host", targetHost)
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		requestsTotal.WithLabelValues("CONNECT", "upstream_error").Inc()
		return
	}
	upstream = wrapIdleTimeout(upstream, h.IdleTimeout)
	defer upstream.Close()
	activeConnections.Inc()
	defer activeConnections.Dec()

	if _, err := fmt.Fprintf(upstream, "CONNECT %s HTTP/1.1\r\n", r.Host); err != nil {
		h.Logger.Error("write CONNECT failed", "error", err)
		return
	}
	if err := writeHeader(upstream, "Host", r.Host); err != nil {
		return
	}
	if h.UpstreamAuth != "" {
		if err := writeHeader(upstream, "Proxy-Authorization", h.UpstreamAuth); err != nil {
			return
		}
	}
	for name, value := range h.ExtraHeaders {
		if err := writeHeader(upstream, name, value); err != nil {
			return
		}
	}
	if _, err := upstream.Write([]byte("\r\n")); err != nil {
		return
	}

	upstreamReader := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamReader, r)
	if err != nil {
		h.Logger.Error("read upstream CONNECT response failed", "error", err)
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		requestsTotal.WithLabelValues("CONNECT", "upstream_error").Inc()
		return
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.Logger.Warn("upstream CONNECT rejected",
			"status", resp.StatusCode,
			"target_host", targetHost,
		)
		_, _ = fmt.Fprintf(clientConn, "HTTP/1.1 %d %s\r\n\r\n", resp.StatusCode, resp.Status)
		requestsTotal.WithLabelValues("CONNECT", "upstream_rejected").Inc()
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}

	// Flush any buffered data from client (e.g. TLS ClientHello already sent).
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buffered, _ := clientBuf.Reader.Peek(clientBuf.Reader.Buffered())
		if _, err := upstream.Write(buffered); err != nil {
			return
		}
		_, _ = clientBuf.Reader.Discard(clientBuf.Reader.Buffered())
	}

	// Wrapping only clientConn here (upstream is already wrapped, from right
	// after dialUpstream) is enough to bound the whole tunnel: each
	// direction's io.Copy inside tunnel() does a Read on one connection and
	// a Write on the other per chunk relayed, so traffic in EITHER direction
	// resets BOTH connections' deadlines -- this behaves as one shared idle
	// timer for the tunnel as a whole, not two independent per-leg timers.
	bytesIn, bytesOut := tunnel(wrapIdleTimeout(clientConn, h.IdleTimeout), upstream)
	bytesTransferred.WithLabelValues("client_to_upstream").Add(float64(bytesOut))
	bytesTransferred.WithLabelValues("upstream_to_client").Add(float64(bytesIn))

	h.Audit.Log(audit.Event{
		Timestamp:     start,
		ClientAddr:    r.RemoteAddr,
		SPIFFE:        h.spiffeFromRequest(r),
		Method:        "CONNECT",
		TargetHost:    targetHost,
		TargetPort:    targetPort,
		UpstreamProxy: h.UpstreamProxy,
		Decision:      "allow",
		Status:        200,
		BytesIn:       bytesIn,
		BytesOut:      bytesOut,
		DurationMS:    time.Since(start).Milliseconds(),
	})
	requestsTotal.WithLabelValues("CONNECT", "allow").Inc()
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// dialUpstream opens a connection to the upstream proxy. When mTLS is enabled
// the connection is wrapped with tls.Client using the current certificate config.
func (h *Handler) dialUpstream() (net.Conn, error) {
	d := &net.Dialer{Timeout: h.DialTimeout}
	raw, err := d.Dial("tcp", h.UpstreamProxy)
	if err != nil {
		return nil, err
	}

	if !h.TLSEnabled {
		return raw, nil
	}

	if h.CertManager == nil {
		raw.Close()
		return nil, errors.New("mTLS enabled but cert manager is nil")
	}

	tlsCfg := h.CertManager.TLSConfig().Clone()
	host, _, _ := net.SplitHostPort(h.UpstreamProxy)
	tlsCfg.ServerName = host
	tlsCfg.InsecureSkipVerify = h.InsecureSkipVerify

	tlsConn := tls.Client(raw, tlsCfg)
	ctx, cancel := context.WithTimeout(context.Background(), h.DialTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return tlsConn, nil
}

func (h *Handler) denyForward(w http.ResponseWriter, r *http.Request, host string, port uint32, reason string) {
	h.Audit.Log(audit.Event{
		Timestamp:  time.Now(),
		ClientAddr: r.RemoteAddr,
		SPIFFE:     h.spiffeFromRequest(r),
		Method:     "HTTP-FORWARD",
		TargetHost: host,
		TargetPort: port,
		Decision:   "deny",
		DenyReason: reason,
		Status:     http.StatusForbidden,
	})
	requestsTotal.WithLabelValues("HTTP", "deny").Inc()
	http.Error(w, fmt.Sprintf("forbidden: %s", reason), http.StatusForbidden)
}

func (h *Handler) denyConnect(w http.ResponseWriter, r *http.Request, host string, port uint32, reason string) {
	h.Audit.Log(audit.Event{
		Timestamp:  time.Now(),
		ClientAddr: r.RemoteAddr,
		SPIFFE:     h.spiffeFromRequest(r),
		Method:     "CONNECT",
		TargetHost: host,
		TargetPort: port,
		Decision:   "deny",
		DenyReason: reason,
		Status:     http.StatusForbidden,
	})
	requestsTotal.WithLabelValues("CONNECT", "deny").Inc()
	http.Error(w, fmt.Sprintf("forbidden: %s", reason), http.StatusForbidden)
}

// tunnel copies data bidirectionally between two connections.
// Returns bytesIn (upstream→client) and bytesOut (client→upstream).
func tunnel(client, upstream net.Conn) (bytesIn, bytesOut int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstream, client)
		bytesOut = n
		if cw, ok := upstream.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, upstream)
		bytesIn = n
		if cw, ok := client.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}()

	wg.Wait()
	return
}

type closeWriter interface {
	CloseWrite() error
}

// wrapIdleTimeout wraps conn so that timeout behaves as a genuine idle
// timeout -- one that fires only after a gap with no activity -- instead of
// a fixed deadline counted from when the connection was opened. A
// non-positive timeout disables it (no deadline at all), matching the
// common Go convention rather than the previous behavior of every
// non-positive value producing an already-expired deadline.
func wrapIdleTimeout(conn net.Conn, timeout time.Duration) net.Conn {
	if timeout <= 0 {
		return conn
	}
	return &idleTimeoutConn{Conn: conn, timeout: timeout}
}

// idleTimeoutConn resets the wrapped connection's deadline to now+timeout
// before every Read and Write, so the deadline only ever expires after a
// genuine gap with no activity on the connection.
//
// net.Conn's deadline API is inherently a fixed point in time, not a
// resettable idle timer -- calling SetDeadline once at connection open (the
// previous approach in handleHTTPForward) makes it fire at that fixed
// offset regardless of how much legitimate traffic is still flowing, e.g.
// cutting off a slow-but-live response body partway through (acceptance
// review finding F9, istio-forward-proxy issue #27). Resetting it on every
// actual Read/Write syscall -- which is exactly when bufio.Reader and
// io.Copy call through to the underlying connection, i.e. exactly when the
// connection was NOT idle -- turns it into the idle timeout the flag name
// promises.
//
// handleConnect wraps both legs of an established CONNECT tunnel with this
// too (finding F10, issue #28): previously neither handleConnect nor
// tunnel() set any deadline at all, so an abandoned tunnel -- opened, then
// never closed cleanly by the client -- pinned a goroutine and two file
// descriptors on this proxy forever. Wrapping both connections is enough to
// bound the tunnel as a whole rather than each leg independently: every
// chunk tunnel() relays does a Read on one connection and a Write on the
// other, so traffic in either direction resets both connections' deadlines.
type idleTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *idleTimeoutConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func writeHeader(w io.Writer, name, value string) error {
	_, err := fmt.Fprintf(w, "%s: %s\r\n", name, value)
	return err
}

// isHopByHop reports whether a header is hop-by-hop and must not be forwarded.
func isHopByHop(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func splitHostPort(hostport string, defaultPort uint32) (string, uint32) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, defaultPort
	}
	p, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return host, defaultPort
	}
	return host, uint32(p)
}

func defaultPortForScheme(scheme string) uint32 {
	if scheme == "https" {
		return 443
	}
	return 80
}

// spiffeFromRequest extracts the SPIFFE URI identifying the caller, for the
// audit log. A verified mTLS peer certificate is always preferred when one
// is present on the connection.
//
// In ambient mode there never is one: ztunnel terminates HBONE mTLS itself
// at L4 and hands this proxy plain TCP, so r.TLS is nil regardless of what
// PeerAuthentication enforces. ztunnel does not parse HTTP, so it neither
// sets nor strips an X-Forwarded-Client-Cert header -- anything a client
// sends there reaches this handler completely unverified. Trusting it by
// default would let any caller dictate its own identity in the audit trail
// (see istio-forward-proxy-acceptance-review finding F1), so it's only
// consulted when h.TrustXFCCHeader is explicitly enabled. Turn that on only
// when something in front of this proxy actually guarantees the header
// can't originate from the client itself -- e.g. an L7-aware hop (a
// waypoint) that sanitizes and re-sets it from its own verified mTLS state.
// Envoy, which this project positions against, defaults to the equivalent
// of this off (forward_client_cert_details: SANITIZE).
func (h *Handler) spiffeFromRequest(r *http.Request) string {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		for _, uri := range r.TLS.PeerCertificates[0].URIs {
			if uri.Scheme == "spiffe" {
				return uri.String()
			}
		}
	}
	if !h.TrustXFCCHeader {
		return ""
	}
	if v := r.Header.Get("X-Forwarded-Client-Cert"); v != "" {
		return extractSPIFFEFromXFCC(v)
	}
	return ""
}

// extractSPIFFEFromXFCC parses the URI="spiffe://..." claim from an XFCC header.
func extractSPIFFEFromXFCC(v string) string {
	const key = "URI=\""
	i := strings.Index(v, key)
	if i < 0 {
		return ""
	}
	rest := v[i+len(key):]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return ""
	}
	u, err := url.Parse(rest[:end])
	if err != nil || u.Scheme != "spiffe" {
		return ""
	}
	return u.String()
}
