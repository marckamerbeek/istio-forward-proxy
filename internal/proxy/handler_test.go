package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestBuildUpstreamRequest covers what writeProxyRequest's dedicated tests
// used to check directly on the raw bytes it wrote to the wire: since
// buildUpstreamRequest instead produces an *http.Request for
// Transport.RoundTrip to write (see buildTransport for why), the
// equivalent guarantees are checked on that request object.
func TestBuildUpstreamRequest(t *testing.T) {
	t.Run("absolute-form URL, Host, and RequestURI", func(t *testing.T) {
		// RFC 7230 §5.3.2: a proxy request's target must be the absolute
		// URI, which is what Transport.Proxy makes Transport write to the
		// upstream -- the core difference from Envoy, which rewrites to a
		// relative path.
		cases := []struct {
			name     string
			inputURL string
			wantURL  string
			wantHost string
		}{
			{"simple GET", "http://edition.cnn.com/politics", "http://edition.cnn.com/politics", "edition.cnn.com"},
			{"with query string", "http://api.example.com/v1/users?page=2&limit=50", "http://api.example.com/v1/users?page=2&limit=50", "api.example.com"},
			{"with explicit port", "http://internal.corp:8080/healthz", "http://internal.corp:8080/healthz", "internal.corp:8080"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest("GET", tc.inputURL, nil)
				h := &Handler{}
				outReq, _, err := h.buildUpstreamRequest(req)
				if err != nil {
					t.Fatalf("buildUpstreamRequest: %v", err)
				}
				if got := outReq.URL.String(); got != tc.wantURL {
					t.Errorf("URL = %q, want %q", got, tc.wantURL)
				}
				if outReq.Host != tc.wantHost {
					t.Errorf("Host = %q, want %q", outReq.Host, tc.wantHost)
				}
				if outReq.RequestURI != "" {
					t.Errorf("RequestURI = %q, want empty (Transport.RoundTrip refuses a request with this set)", outReq.RequestURI)
				}
			})
		}
	})

	t.Run("Proxy-Authorization is ours, not the client's", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		req.Header.Set("Proxy-Authorization", "Basic shouldbeoverwritten")
		h := &Handler{UpstreamAuth: "Basic correct"}
		outReq, _, err := h.buildUpstreamRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := outReq.Header.Get("Proxy-Authorization"); got != "Basic correct" {
			t.Errorf("Proxy-Authorization = %q, want %q (client-supplied value must not leak upstream)", got, "Basic correct")
		}
	})

	t.Run("hop-by-hop headers stripped, end-to-end headers kept", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		req.Header.Set("Connection", "close")
		req.Header.Set("Keep-Alive", "timeout=5")
		req.Header.Set("X-Custom-App", "keepme")
		h := &Handler{}
		outReq, _, err := h.buildUpstreamRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if outReq.Header.Get("Connection") != "" {
			t.Error("Connection header should have been stripped")
		}
		if outReq.Header.Get("Keep-Alive") != "" {
			t.Error("Keep-Alive header should have been stripped")
		}
		if got := outReq.Header.Get("X-Custom-App"); got != "keepme" {
			t.Errorf("end-to-end header X-Custom-App = %q, want %q", got, "keepme")
		}
	})

	t.Run("extra headers set", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		h := &Handler{ExtraHeaders: map[string]string{
			"X-Corp-Id":    "corp-123",
			"X-Request-By": "forward-proxy",
		}}
		outReq, _, err := h.buildUpstreamRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := outReq.Header.Get("X-Corp-Id"); got != "corp-123" {
			t.Errorf("X-Corp-Id = %q, want %q", got, "corp-123")
		}
		if got := outReq.Header.Get("X-Request-By"); got != "forward-proxy" {
			t.Errorf("X-Request-By = %q, want %q", got, "forward-proxy")
		}
	})
}

// countingListener counts accepted TCP connections, to prove pooling
// actually happens (below) rather than just trusting Transport's
// documented behavior.
type countingListener struct {
	net.Listener
	accepts atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return conn, err
}

// TestUpstreamPoolReusesConnections verifies the fix for
// istio-forward-proxy-acceptance-review finding F11: previously every
// request dialed a fresh connection to the upstream (no http.Transport or
// http.Client anywhere in the codebase), measured in the review at +72%
// latency per request on the plain HTTP path. buildTransport routes
// requests through an http.Transport instead, which pools and reuses
// connections to the upstream automatically.
//
// A real net.Listener (via httptest.NewUnstartedServer, wrapped to count
// Accept calls) stands in as the upstream: N sequential requests, each
// fully drained and closed before the next starts, must reuse the same
// pooled connection rather than dialing fresh each time.
func TestUpstreamPoolReusesConnections(t *testing.T) {
	var requestsSeen atomic.Int64
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsSeen.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	cl := &countingListener{Listener: upstream.Listener}
	upstream.Listener = cl
	upstream.Start()
	defer upstream.Close()

	h := &Handler{
		UpstreamProxy: strings.TrimPrefix(upstream.URL, "http://"),
		DialTimeout:   time.Second,
		IdleTimeout:   time.Second,
	}

	const n = 10
	for i := 0; i < n; i++ {
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		outReq, _, err := h.buildUpstreamRequest(req)
		if err != nil {
			t.Fatalf("request %d: buildUpstreamRequest: %v", i, err)
		}
		resp, err := h.upstreamTransport().RoundTrip(outReq)
		if err != nil {
			t.Fatalf("request %d: RoundTrip: %v", i, err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("request %d: draining body: %v", i, err)
		}
		resp.Body.Close()
	}

	if got := requestsSeen.Load(); got != n {
		t.Fatalf("upstream saw %d requests, want %d", got, n)
	}
	if accepts := cl.accepts.Load(); accepts != 1 {
		t.Errorf("upstream accepted %d TCP connections for %d sequential requests, want 1 (connections should be pooled and reused)", accepts, n)
	}
}

// TestUpstreamChunkedBodyRelayedIntact verifies that a chunked request body
// still survives the hop to the upstream via the http.Transport-based path
// (finding F2, issue #26, originally fixed by hand-reframing the body in
// writeProxyRequest -- now handled natively by Transport, since
// buildUpstreamRequest carries over ContentLength -1 unchanged and
// Transport chunk-encodes an unknown-length body correctly by
// construction).
//
// req is built via http.ReadRequest from a raw chunked request, exactly as
// net/http hands it to a real server, to exercise the real production
// input shape rather than a synthetic stand-in.
func TestUpstreamChunkedBodyRelayedIntact(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream: reading body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	raw := "POST http://echo-origin.example/echo HTTP/1.1\r\n" +
		"Host: echo-origin.example\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"\r\n" +
		"5\r\nhello\r\n0\r\n\r\n"
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("constructing test request: %v", err)
	}
	if req.ContentLength != -1 {
		t.Fatalf("test premise broken: expected ContentLength -1 for a chunked request, got %d", req.ContentLength)
	}

	h := &Handler{
		UpstreamProxy: strings.TrimPrefix(upstream.URL, "http://"),
		DialTimeout:   time.Second,
		IdleTimeout:   time.Second,
	}
	outReq, _, err := h.buildUpstreamRequest(req)
	if err != nil {
		t.Fatalf("buildUpstreamRequest: %v", err)
	}
	resp, err := h.upstreamTransport().RoundTrip(outReq)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if string(gotBody) != "hello" {
		t.Errorf("upstream received body %q, want %q", gotBody, "hello")
	}
}

// TestClientToUpstreamBytesCounted guards against a regression this change
// nearly introduced silently: writeProxyRequest used to count outbound
// body bytes itself (for forward_proxy_bytes_transferred_total{direction=
// "client_to_upstream"}) as it wrote them; Transport now writes the body
// internally, so buildUpstreamRequest wraps it in a countingReadCloser
// instead. This checks that wrapper actually counts what Transport reads
// from it over a real round trip, not just in isolation.
func TestClientToUpstreamBytesCounted(t *testing.T) {
	const body = "hello world"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	req := httptest.NewRequest("POST", "http://example.com/", strings.NewReader(body))
	h := &Handler{
		UpstreamProxy: strings.TrimPrefix(upstream.URL, "http://"),
		DialTimeout:   time.Second,
		IdleTimeout:   time.Second,
	}
	outReq, bodyCounter, err := h.buildUpstreamRequest(req)
	if err != nil {
		t.Fatalf("buildUpstreamRequest: %v", err)
	}
	if bodyCounter == nil {
		t.Fatal("expected a non-nil body counter for a request with a body")
	}

	resp, err := h.upstreamTransport().RoundTrip(outReq)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if got := bodyCounter.Load(); got != int64(len(body)) {
		t.Errorf("bodyCounter.Load() = %d, want %d", got, len(body))
	}
}

// TestNoBodyCounterForBodylessRequest verifies buildUpstreamRequest returns
// a nil counter for a request with no body, so handleHTTPForward's
// bytesTransferred.Add call is skipped rather than adding a bogus zero.
func TestNoBodyCounterForBodylessRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	h := &Handler{}
	_, bodyCounter, err := h.buildUpstreamRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if bodyCounter != nil {
		t.Error("expected a nil body counter for a bodyless request")
	}
}

// TestAuditIdentityNotForgeable verifies the fix for
// istio-forward-proxy-acceptance-review finding F1: a client could dictate
// its own SPIFFE identity in the audit log via a self-supplied
// X-Forwarded-Client-Cert header, since in ambient mode there's never a
// verified mTLS peer certificate to check it against (ztunnel terminates
// mTLS at L4 and doesn't touch this header). The header must now be ignored
// unless the operator explicitly opts in via TrustXFCCHeader, and a real
// verified peer certificate must always take precedence over it either way.
func TestAuditIdentityNotForgeable(t *testing.T) {
	const forgedHeader = `By=spiffe://cluster.local/ns/istio-egress/sa/forward-proxy;Hash=abc123;URI="spiffe://cluster.local/ns/kube-system/sa/cluster-admin"`

	t.Run("XFCC header ignored by default", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		req.Header.Set("X-Forwarded-Client-Cert", forgedHeader)

		h := &Handler{}
		if got := h.spiffeFromRequest(req); got != "" {
			t.Errorf("client-supplied XFCC header was trusted by default: got %q, want \"\"", got)
		}
	})

	t.Run("XFCC header used once explicitly trusted", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		req.Header.Set("X-Forwarded-Client-Cert", forgedHeader)

		h := &Handler{TrustXFCCHeader: true}
		want := "spiffe://cluster.local/ns/kube-system/sa/cluster-admin"
		if got := h.spiffeFromRequest(req); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("verified peer certificate always wins over the header", func(t *testing.T) {
		certURI, err := url.Parse("spiffe://cluster.local/ns/team-a/sa/app-x")
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		req.Header.Set("X-Forwarded-Client-Cert", forgedHeader)
		req.TLS = &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{certURI}}},
		}

		for _, trust := range []bool{false, true} {
			h := &Handler{TrustXFCCHeader: trust}
			want := "spiffe://cluster.local/ns/team-a/sa/app-x"
			if got := h.spiffeFromRequest(req); got != want {
				t.Errorf("TrustXFCCHeader=%v: got %q, want %q (verified cert should always win)", trust, got, want)
			}
		}
	})
}

// TestIdleTimeoutSurvivesSlowButLiveTransfer verifies the fix for
// istio-forward-proxy-acceptance-review finding F9: the deadline behind
// --idle-timeout used to be set exactly once, at connection open, so it
// fired at that fixed offset regardless of ongoing activity -- cutting off
// a slow-but-live transfer partway through with no error status, just fewer
// bytes than promised. With idleTimeoutConn, every actual Read resets the
// deadline, so a transfer whose gaps between chunks all stay under the
// idle timeout must survive even though its total duration exceeds it many
// times over.
func TestIdleTimeoutSurvivesSlowButLiveTransfer(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()

	const (
		idle       = 40 * time.Millisecond
		gap        = idle / 2 // well under idle, so activity keeps resetting the deadline
		chunkCount = 5
	)

	go func() {
		defer server.Close()
		for i := 0; i < chunkCount; i++ {
			time.Sleep(gap)
			if _, err := server.Write([]byte{byte(i)}); err != nil {
				return
			}
		}
	}()

	wrapped := wrapIdleTimeout(client, idle)
	defer wrapped.Close()

	buf := make([]byte, 1)
	for i := 0; i < chunkCount; i++ {
		n, err := wrapped.Read(buf)
		if err != nil {
			t.Fatalf("chunk %d: read failed even though every gap (%s) was under the idle timeout (%s): %v", i, gap, idle, err)
		}
		if n != 1 || buf[0] != byte(i) {
			t.Fatalf("chunk %d: got %v, want [%d]", i, buf[:n], i)
		}
	}
}

// TestIdleTimeoutFiresOnGenuineIdle verifies the other half of the F9 fix:
// resetting the deadline on activity must not turn it into "no timeout at
// all" -- a connection with no activity whatsoever must still time out.
func TestIdleTimeoutFiresOnGenuineIdle(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	wrapped := wrapIdleTimeout(client, 20*time.Millisecond)
	defer wrapped.Close()

	_, err := wrapped.Read(make([]byte, 1)) // server never writes anything
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("expected a timeout error on genuine idle, got %v", err)
	}
}

// TestWrapIdleTimeoutDisabledForNonPositive verifies that a non-positive
// timeout disables the wrapper entirely (no deadline at all), rather than
// reproducing the pre-fix bug where time.Now().Add(0) produced a deadline
// that had already expired.
func TestWrapIdleTimeoutDisabledForNonPositive(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	if wrapped := wrapIdleTimeout(client, 0); wrapped != client {
		t.Error("timeout <= 0 should return the connection unwrapped")
	}
}

// TestConnectTunnelClosesOnGenuineIdle verifies the fix for
// istio-forward-proxy-acceptance-review finding F10: neither handleConnect
// nor tunnel() set any deadline anywhere on an established CONNECT tunnel,
// so an abandoned tunnel -- opened, then never closed cleanly by the client
// -- pinned a goroutine and two file descriptors on this proxy forever.
// Wrapping both legs with wrapIdleTimeout (as handleConnect now does) must
// make tunnel() return on its own once both sides go genuinely idle,
// instead of blocking indefinitely.
func TestConnectTunnelClosesOnGenuineIdle(t *testing.T) {
	clientPeer, clientEnd := net.Pipe()
	defer clientPeer.Close()
	upstreamPeer, upstreamEnd := net.Pipe()
	defer upstreamPeer.Close()

	const idle = 30 * time.Millisecond

	done := make(chan struct{})
	go func() {
		tunnel(wrapIdleTimeout(clientEnd, idle), wrapIdleTimeout(upstreamEnd, idle))
		close(done)
	}()

	select {
	case <-done:
		// An idle tunnel closed itself, as it must.
	case <-time.After(idle * 20):
		t.Fatal("tunnel() never returned for a fully idle tunnel (F10 regression)")
	}
}

// TestConnectTunnelSurvivesOneDirectionalActivity verifies that wrapping
// both tunnel legs independently doesn't degrade into two independent
// per-leg timers: a tunnel carrying continuous traffic in only ONE
// direction (e.g. a client upload where the origin never talks back) must
// not be torn down just because the quiet leg's own reads are idle. Each
// chunk tunnel() relays does a Read on one connection and a Write on the
// other, so activity in either direction resets both connections'
// deadlines -- this is what makes the wrapping behave as one shared idle
// timer for the tunnel as a whole.
func TestConnectTunnelSurvivesOneDirectionalActivity(t *testing.T) {
	clientPeer, clientEnd := net.Pipe()
	upstreamPeer, upstreamEnd := net.Pipe()

	const (
		idle  = 60 * time.Millisecond
		gap   = 15 * time.Millisecond // well under idle
		round = 8                     // round*gap = 120ms, 2x idle
	)

	done := make(chan struct{})
	go func() {
		tunnel(wrapIdleTimeout(clientEnd, idle), wrapIdleTimeout(upstreamEnd, idle))
		close(done)
	}()
	go func() { _, _ = io.Copy(io.Discard, upstreamPeer) }() // drain, so client writes never block

	for i := 0; i < round; i++ {
		select {
		case <-done:
			t.Fatalf("tunnel closed after %d/%d rounds of sustained one-directional activity", i, round)
		default:
		}
		time.Sleep(gap)
		if _, err := clientPeer.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("round %d: client write failed, tunnel closed early: %v", i, err)
		}
	}

	clientPeer.Close()
	upstreamPeer.Close()
	select {
	case <-done:
	case <-time.After(idle * 10):
		t.Fatal("tunnel never closed after both peers disconnected")
	}
}

// TestNonAbsoluteURIRejected verifies that non-proxy requests receive a 400.
func TestNonAbsoluteURIRejected(t *testing.T) {
	req := httptest.NewRequest("GET", "/relative", nil)
	req.URL.Scheme = ""
	req.URL.Host = ""

	h := &Handler{}
	w := httptest.NewRecorder()
	h.handleHTTPForward(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
