package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestAbsolutePathPreservation verifies the core property of this proxy:
// the request-line sent to the upstream preserves the ABSOLUTE URI as
// required by RFC 7230 §5.3.2 for proxy requests.
func TestAbsolutePathPreservation(t *testing.T) {
	cases := []struct {
		name     string
		inputURL string
		method   string
		wantLine string
	}{
		{
			name:     "simple GET",
			inputURL: "http://edition.cnn.com/politics",
			method:   "GET",
			wantLine: "GET http://edition.cnn.com/politics HTTP/1.1",
		},
		{
			name:     "with query string",
			inputURL: "http://api.example.com/v1/users?page=2&limit=50",
			method:   "GET",
			wantLine: "GET http://api.example.com/v1/users?page=2&limit=50 HTTP/1.1",
		},
		{
			name:     "with explicit port",
			inputURL: "http://internal.corp:8080/healthz",
			method:   "GET",
			wantLine: "GET http://internal.corp:8080/healthz HTTP/1.1",
		},
		{
			name:     "POST request",
			inputURL: "http://api.example.com/submit",
			method:   "POST",
			wantLine: "POST http://api.example.com/submit HTTP/1.1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.inputURL, nil)
			var buf strings.Builder
			h := &Handler{UpstreamAuth: ""}
			if err := h.writeProxyRequest(&nopWriteConn{Builder: &buf}, req); err != nil {
				t.Fatalf("writeProxyRequest: %v", err)
			}

			gotFirstLine := strings.SplitN(buf.String(), "\r\n", 2)[0]
			if gotFirstLine != tc.wantLine {
				t.Errorf("request-line = %q, want %q", gotFirstLine, tc.wantLine)
			}
		})
	}
}

// TestProxyAuthorizationInjected verifies that the Proxy-Authorization header
// is correctly added to upstream requests.
func TestProxyAuthorizationInjected(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	h := &Handler{UpstreamAuth: "Basic dXNlcjpwYXNz"}

	var buf strings.Builder
	if err := h.writeProxyRequest(&nopWriteConn{Builder: &buf}, req); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "Proxy-Authorization: Basic dXNlcjpwYXNz\r\n") {
		t.Errorf("Proxy-Authorization not found in:\n%s", buf.String())
	}
}

// TestHopByHopHeadersStripped verifies that hop-by-hop headers from the client
// are not forwarded to the upstream.
func TestHopByHopHeadersStripped(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("Connection", "close")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Authorization", "Basic shouldbeoverwritten")
	req.Header.Set("X-Custom-App", "keepme")

	h := &Handler{UpstreamAuth: "Basic correct"}
	var buf strings.Builder
	if err := h.writeProxyRequest(&nopWriteConn{Builder: &buf}, req); err != nil {
		t.Fatal(err)
	}

	s := buf.String()
	if strings.Contains(s, "Connection: close") {
		t.Error("Connection header should have been stripped")
	}
	if strings.Contains(s, "Keep-Alive:") {
		t.Error("Keep-Alive header should have been stripped")
	}
	if !strings.Contains(s, "Proxy-Authorization: Basic correct") {
		t.Error("Proxy-Authorization should be our own, not client's")
	}
	if strings.Contains(s, "Basic shouldbeoverwritten") {
		t.Error("Client's Proxy-Authorization leaked into upstream")
	}
	if !strings.Contains(s, "X-Custom-App: keepme") {
		t.Error("End-to-end custom header should be preserved")
	}
}

// TestExtraHeadersAppended verifies that configured extra headers are forwarded.
func TestExtraHeadersAppended(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	h := &Handler{
		ExtraHeaders: map[string]string{
			"X-Corp-Id":    "corp-123",
			"X-Request-By": "forward-proxy",
		},
	}
	var buf strings.Builder
	if err := h.writeProxyRequest(&nopWriteConn{Builder: &buf}, req); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, "X-Corp-Id: corp-123\r\n") {
		t.Error("X-Corp-Id not found")
	}
	if !strings.Contains(s, "X-Request-By: forward-proxy\r\n") {
		t.Error("X-Request-By not found")
	}
}

// TestHostHeaderSet verifies that the Host header is set correctly.
func TestHostHeaderSet(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com:8080/path", nil)
	h := &Handler{}
	var buf strings.Builder
	if err := h.writeProxyRequest(&nopWriteConn{Builder: &buf}, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(strings.NewReader(buf.String()))
	_, _ = br.ReadString('\n') // skip request-line
	line, _ := br.ReadString('\n')
	if !strings.HasPrefix(strings.TrimSpace(line), "Host:") {
		t.Errorf("expected Host header first, got: %q", line)
	}
	if !strings.Contains(line, "example.com:8080") {
		t.Errorf("Host header wrong: %q", line)
	}
}

// TestChunkedRequestBodyReframed verifies the fix for a request-smuggling
// primitive (istio-forward-proxy-acceptance-review finding F2): a client
// request with Transfer-Encoding: chunked was forwarded with neither
// Transfer-Encoding nor Content-Length, so the upstream read it as a
// zero-length body and parsed the still-arriving body bytes as the start of
// the next request on the same connection.
//
// req is built via http.ReadRequest from a raw chunked request, exactly as
// net/http hands it to a real server: Transfer-Encoding is already stripped
// out of req.Header (moved to req.TransferEncoding) and req.ContentLength is
// -1, so this exercises the real production code path rather than a
// synthetic stand-in.
func TestChunkedRequestBodyReframed(t *testing.T) {
	raw := "POST http://echo-origin.echo-origin.svc.cluster.local/echo HTTP/1.1\r\n" +
		"Host: echo-origin.echo-origin.svc.cluster.local\r\n" +
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
	if _, present := req.Header["Transfer-Encoding"]; present {
		t.Fatalf("test premise broken: expected net/http to strip Transfer-Encoding out of Header")
	}

	var buf strings.Builder
	h := &Handler{}
	if err := h.writeProxyRequest(&nopWriteConn{Builder: &buf}, req); err != nil {
		t.Fatalf("writeProxyRequest: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "Transfer-Encoding: chunked\r\n") {
		t.Fatalf("upstream request has no body-framing header at all:\n%s", out)
	}

	// The upstream sees exactly this: parse it back with a real HTTP parser,
	// the way a proxy or origin one hop further out would.
	br := bufio.NewReader(strings.NewReader(out))
	parsed, err := http.ReadRequest(br)
	if err != nil {
		t.Fatalf("upstream could not parse the forwarded request: %v\nwire bytes:\n%s", err, out)
	}
	body, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatalf("upstream could not read the forwarded body: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}

	// Nothing must be left on the connection after the framed request+body:
	// that's exactly the smuggled bytes a next hop would read as a new
	// request-line.
	if _, err := br.ReadByte(); err != io.EOF {
		t.Errorf("trailing bytes after the framed request (would be parsed as the next request-line), read err = %v", err)
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

// nopWriteConn satisfies io.Writer for writeProxyRequest tests.
type nopWriteConn struct {
	*strings.Builder
}

func (n *nopWriteConn) Write(p []byte) (int, error) {
	return n.Builder.Write(p)
}
