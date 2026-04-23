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
	Logger             *slog.Logger
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
	defer upstream.Close()
	activeConnections.Inc()
	defer activeConnections.Dec()

	if err := upstream.SetDeadline(time.Now().Add(h.IdleTimeout)); err != nil {
		h.Logger.Debug("set deadline failed", "error", err)
	}

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
		SPIFFE:        spiffeFromRequest(r),
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

	if _, err := w.Write([]byte("\r\n")); err != nil {
		return err
	}

	if r.Body != nil && r.Body != http.NoBody {
		n, err := io.Copy(w, r.Body)
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

	bytesIn, bytesOut := tunnel(clientConn, upstream)
	bytesTransferred.WithLabelValues("client_to_upstream").Add(float64(bytesOut))
	bytesTransferred.WithLabelValues("upstream_to_client").Add(float64(bytesIn))

	h.Audit.Log(audit.Event{
		Timestamp:     start,
		ClientAddr:    r.RemoteAddr,
		SPIFFE:        spiffeFromRequest(r),
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
		SPIFFE:     spiffeFromRequest(r),
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
		SPIFFE:     spiffeFromRequest(r),
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

// spiffeFromRequest extracts the SPIFFE URI from the client certificate as
// forwarded by ztunnel. In ambient mode ztunnel initiates the HBONE mTLS
// connection and presents the pod's SPIFFE identity.
//
// When the proxy receives plain HTTP after HBONE decryption by ztunnel, the
// identity may be forwarded in the X-Forwarded-Client-Cert header via a pod
// label or custom authz filter.
func spiffeFromRequest(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		if v := r.Header.Get("X-Forwarded-Client-Cert"); v != "" {
			return extractSPIFFEFromXFCC(v)
		}
		return ""
	}
	for _, uri := range r.TLS.PeerCertificates[0].URIs {
		if uri.Scheme == "spiffe" {
			return uri.String()
		}
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
