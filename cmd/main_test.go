package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/marckamerbeek/istio-forward-proxy/internal/serviceentry"
)

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		want       bool
	}{
		{"ipv4 loopback", "127.0.0.1:54321", true},
		{"ipv6 loopback", "[::1]:54321", true},
		{"ipv4 non-loopback", "10.1.2.3:54321", false},
		{"pod ip, no relation to loopback", "192.168.1.5:8080", false},
		{"loopback without port", "127.0.0.1", true},
		{"malformed", "not-an-ip", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLoopback(tc.remoteAddr); got != tc.want {
				t.Errorf("isLoopback(%q) = %v, want %v", tc.remoteAddr, got, tc.want)
			}
		})
	}
}

// newTestWatcher builds a serviceentry.Watcher without a real cluster — it
// never calls Run(), so the unreachable kubeconfig is never dialed. This
// mirrors scripts/local-test.sh's fake-kubeconfig approach.
func newTestWatcher(t *testing.T) *serviceentry.Watcher {
	t.Helper()

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig.fake")
	fake := `apiVersion: v1
kind: Config
clusters: [{name: none, cluster: {server: https://127.0.0.1:1}}]
contexts: [{name: none, context: {cluster: none, user: none}}]
current-context: none
users: [{name: none}]
`
	if err := os.WriteFile(kubeconfig, []byte(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	w, err := serviceentry.NewWatcher(logger)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	return w
}

func TestDebugAllowlistRestrictedToLoopback(t *testing.T) {
	mux := metricsMux(newTestWatcher(t))

	cases := []struct {
		name       string
		remoteAddr string
		wantStatus int
	}{
		{"loopback (kubectl exec) allowed", "127.0.0.1:12345", http.StatusOK},
		{"loopback via IPv6 allowed", "[::1]:12345", http.StatusOK},
		{"another pod on the network blocked", "10.42.0.7:53211", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/debug/allowlist", nil)
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

// TestHealthzUnaffectedByLoopbackGate proves the loopback restriction is
// scoped to /debug/allowlist only — /healthz must stay reachable from
// anywhere on the network for kubelet probes.
func TestHealthzUnaffectedByLoopbackGate(t *testing.T) {
	mux := metricsMux(newTestWatcher(t))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "10.42.0.7:53211" // a node/kubelet IP, not loopback
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want %d", rec.Code, http.StatusOK)
	}
}
