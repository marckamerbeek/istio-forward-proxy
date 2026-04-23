// Package proxy implementeert de core forward proxy logica. Het ondersteunt:
//
//  1. HTTP forward met absolute path preservatie. De proxy ontvangt een
//     request met absolute URI (GET http://host/path HTTP/1.1), valideert
//     de host tegen de ACL, opent een mTLS verbinding naar de upstream
//     proxy en stuurt het request door met absolute URI behouden.
//
//  2. CONNECT tunnel. Voor HTTPS verkeer krijgt de proxy een CONNECT
//     request. Na ACL check wordt een mTLS verbinding naar de upstream
//     geopend, de CONNECT wordt doorgestuurd, en na 200 Connection
//     established tunnelen we TCP bidirectioneel.
//
// Het cruciale verschil met Envoy's TLS origination is dat wij de absolute
// request-line NIET herschrijven naar een relatief path. Envoy doet dat per
// RFC 7230 §5.3.1 voor origin servers; wij behouden het absoluut pad omdat
// de upstream een proxy is (RFC 7230 §5.3.2).
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

	"github.com/example/istio-forward-proxy/internal/audit"
	"github.com/example/istio-forward-proxy/internal/certs"
	"github.com/example/istio-forward-proxy/internal/serviceentry"

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

// Handler implementeert http.Handler. Het dispatched naar de juiste flow
// op basis van de request methode (CONNECT vs anders).
type Handler struct {
	UpstreamProxy      string // host:port van upstream proxy
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
// HTTP forward met absolute path preservatie
// -----------------------------------------------------------------------------

func (h *Handler) handleHTTPForward(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Een forward proxy request MOET een absolute URI hebben (RFC 7230 §5.3.2).
	// We checken dat de URL scheme heeft (http/https) en een Host.
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "proxy request requires absolute URI", http.StatusBadRequest)
		requestsTotal.WithLabelValues("HTTP", "bad_request").Inc()
		return
	}

	targetHost, targetPort := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))

	// ACL check via ServiceEntry allowlist
	allow, _ := h.ACL.AllowHost(targetHost, targetPort)
	if !allow {
		h.denyForward(w, r, targetHost, targetPort, "host_not_in_service_entry_allowlist")
		return
	}

	// Connect naar upstream proxy
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

	// Schrijf request naar upstream met ABSOLUUT pad. Dit is de kern van wat
	// Envoy fout doet: Envoy zou schrijven:
	//   GET /path HTTP/1.1
	// Wij schrijven:
	//   GET http://host:port/path HTTP/1.1
	// Dit is conform RFC 7230 §5.3.2 (absolute-form, vereist voor requests
	// naar een proxy).
	if err := h.writeProxyRequest(upstream, r); err != nil {
		h.Logger.Error("write upstream request failed", "error", err)
		http.Error(w, "upstream write failed", http.StatusBadGateway)
		requestsTotal.WithLabelValues("HTTP", "upstream_error").Inc()
		return
	}

	// Lees response van upstream en kopieer naar client. Gebruik een bufio
	// reader om http.ReadResponse te kunnen gebruiken; die parseert headers.
	br := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(br, r)
	if err != nil {
		h.Logger.Error("read upstream response failed", "error", err)
		http.Error(w, "upstream read failed", http.StatusBadGateway)
		requestsTotal.WithLabelValues("HTTP", "upstream_error").Inc()
		return
	}
	defer resp.Body.Close()

	// Kopieer headers en status naar client
	for k, vv := range resp.Header {
		// Hop-by-hop headers niet doorzetten
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

// writeProxyRequest schrijft een HTTP/1.1 request naar de upstream connection
// met de ABSOLUTE URI intact. Dit is het verschil met Go's standaard
// http.Client die absolute URIs herschrijft.
func (h *Handler) writeProxyRequest(w io.Writer, r *http.Request) error {
	// Reconstrueer absolute URI. r.URL.String() behoudt scheme + host + path.
	absURI := r.URL.String()

	if _, err := fmt.Fprintf(w, "%s %s HTTP/1.1\r\n", r.Method, absURI); err != nil {
		return err
	}

	// Verzamel headers. We zetten eerst user headers door, dan onze proxy
	// headers bovenop (Proxy-Authorization, extras). Host wordt altijd gezet.
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

	// User headers behalve hop-by-hop en Host (die hebben we al gezet).
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

	// Headers einde
	if _, err := w.Write([]byte("\r\n")); err != nil {
		return err
	}

	// Body doorkopieren
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
// CONNECT tunnel voor HTTPS verkeer
// -----------------------------------------------------------------------------

func (h *Handler) handleConnect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// r.Host bevat "example.com:443" bij CONNECT
	targetHost, targetPort := splitHostPort(r.Host, 443)

	allow, _ := h.ACL.AllowHost(targetHost, targetPort)
	if !allow {
		h.denyConnect(w, r, targetHost, targetPort, "host_not_in_service_entry_allowlist")
		return
	}

	// Hijack de client connection voor raw tunneling
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

	// Connect naar upstream proxy
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

	// Stuur CONNECT naar upstream met auth headers
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

	// Lees upstream response status
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

	// Succes: signaleer client en tunnel TCP bidirectioneel
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}

	// Flush any buffered data from client (maybe TLS ClientHello al verstuurd)
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buffered, _ := clientBuf.Reader.Peek(clientBuf.Reader.Buffered())
		if _, err := upstream.Write(buffered); err != nil {
			return
		}
		_, _ = clientBuf.Reader.Discard(clientBuf.Reader.Buffered())
	}

	// Bidirectionele kopie
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

// dialUpstream opent een verbinding naar de upstream proxy. Als mTLS is
// enabled wrappen we met tls.Client gebruikmakend van de huidige cert config.
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
	// Server name voor SNI + hostname validatie
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

// tunnel kopieert data bidirectioneel tussen twee connections. Returnt
// bytesIn (upstream->client) en bytesOut (client->upstream).
func tunnel(client, upstream net.Conn) (bytesIn, bytesOut int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstream, client)
		bytesOut = n
		// Signaleer EOF naar upstream
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

// writeHeader schrijft een HTTP header regel.
func writeHeader(w io.Writer, name, value string) error {
	_, err := fmt.Fprintf(w, "%s: %s\r\n", name, value)
	return err
}

// isHopByHop returnt true als een header hop-by-hop is (niet end-to-end).
// Deze moeten we NIET doorzetten naar upstream (behalve Proxy-Authorization
// die we expliciet zelf toevoegen).
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

// spiffeFromRequest probeert de SPIFFE URI te halen uit de client cert
// zoals aangeleverd door ztunnel. In ambient mode initieert ztunnel de
// HBONE mTLS verbinding en presenteert het pod's SPIFFE identiteit.
// Als de forward proxy achter een TLS terminator draait vinden we de
// identiteit in r.TLS.PeerCertificates[0].URIs.
//
// Als de proxy direct plain HTTP van ztunnel ontvangt (plaintext na HBONE
// decryptie door ztunnel), staat de SPIFFE identiteit typisch in een
// header. Ztunnel zet deze niet standaard — je kunt dit aanzetten via
// een pod label of via een custom authz filter. Voor nu returnen we leeg
// als er geen TLS peer cert is.
func spiffeFromRequest(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		// Fallback: check header (kan door een sidecar/ztunnel gezet zijn)
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

// extractSPIFFEFromXFCC parseert de URI="spiffe://..." claim uit een
// XFCC header. Envoy/ztunnel zet deze soms als een extra layer.
func extractSPIFFEFromXFCC(v string) string {
	// Simple parser: zoek URI="spiffe://..."
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
