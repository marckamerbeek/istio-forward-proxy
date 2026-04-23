// Package main is the entrypoint for the istio-forward-proxy.
//
// Dit programma implementeert een forward proxy die het gedrag nabootst van
// Istio's TLS origination via DestinationRule + ServiceEntry, maar met één
// cruciaal verschil: het behoudt het absolute pad in HTTP request-lines zodat
// downstream Squid/HTTP proxies er correct mee om kunnen gaan.
//
// De proxy heeft twee modi:
//  1. Plain HTTP forward met mTLS origination naar upstream (absolute path)
//  2. CONNECT tunnel forwarding voor HTTPS verkeer met mTLS naar upstream
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/istio-forward-proxy/internal/audit"
	"github.com/example/istio-forward-proxy/internal/certs"
	"github.com/example/istio-forward-proxy/internal/proxy"
	"github.com/example/istio-forward-proxy/internal/serviceentry"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	cfg := parseFlags()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.logLevel),
	}))
	slog.SetDefault(logger)

	logger.Info("starting istio-forward-proxy",
		"listen_addr", cfg.listenAddr,
		"upstream_proxy", cfg.upstreamProxy,
		"mtls_enabled", cfg.mtlsEnabled,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Certificate manager: leest Secret met client cert + CA, watcht voor rotatie
	var certManager *certs.Manager
	if cfg.mtlsEnabled {
		var err error
		certManager, err = certs.NewManager(cfg.certDir, logger)
		if err != nil {
			logger.Error("failed to initialize certificate manager", "error", err)
			os.Exit(1)
		}
		go certManager.Watch(ctx)
	}

	// ServiceEntry watcher: bouwt ACL whitelist
	seWatcher, err := serviceentry.NewWatcher(logger)
	if err != nil {
		logger.Error("failed to initialize ServiceEntry watcher", "error", err)
		os.Exit(1)
	}
	go seWatcher.Run(ctx)

	// Audit logger
	auditLogger := audit.New(logger)

	// Proxy handler
	handler := &proxy.Handler{
		UpstreamProxy:     cfg.upstreamProxy,
		UpstreamAuth:      cfg.upstreamAuth,
		CertManager:       certManager,
		ACL:               seWatcher,
		Audit:             auditLogger,
		ExtraHeaders:      cfg.extraHeaders,
		DialTimeout:       cfg.dialTimeout,
		IdleTimeout:       cfg.idleTimeout,
		TLSEnabled:        cfg.mtlsEnabled,
		InsecureSkipVerify: cfg.insecureSkipVerify,
		Logger:            logger,
	}

	// Metrics server op aparte poort
	metricsServer := &http.Server{
		Addr:    cfg.metricsAddr,
		Handler: metricsMux(seWatcher),
	}
	go func() {
		logger.Info("metrics server listening", "addr", cfg.metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	// Hoofdproxy server
	proxyServer := &http.Server{
		Addr:         cfg.listenAddr,
		Handler:      handler,
		ReadTimeout:  cfg.readTimeout,
		WriteTimeout: cfg.writeTimeout,
	}

	go func() {
		logger.Info("proxy server listening", "addr", cfg.listenAddr)
		if err := proxyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("proxy server failed", "error", err)
			cancel()
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig)
	case <-ctx.Done():
		logger.Info("context cancelled")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	_ = proxyServer.Shutdown(shutdownCtx)
	_ = metricsServer.Shutdown(shutdownCtx)

	logger.Info("shutdown complete")
}

func metricsMux(seWatcher *serviceentry.Watcher) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !seWatcher.Synced() {
			http.Error(w, "ServiceEntry cache not synced", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	// Debug endpoint: toont huidige allowlist
	mux.HandleFunc("/debug/allowlist", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		seWatcher.DumpJSON(w)
	})
	return mux
}

type config struct {
	listenAddr         string
	metricsAddr        string
	upstreamProxy      string
	upstreamAuth       string
	certDir            string
	logLevel           string
	extraHeaders       map[string]string
	dialTimeout        time.Duration
	idleTimeout        time.Duration
	readTimeout        time.Duration
	writeTimeout       time.Duration
	mtlsEnabled        bool
	insecureSkipVerify bool
}

func parseFlags() *config {
	cfg := &config{
		extraHeaders: make(map[string]string),
	}

	flag.StringVar(&cfg.listenAddr, "listen", envOr("LISTEN_ADDR", ":3128"),
		"address to listen on for proxy connections")
	flag.StringVar(&cfg.metricsAddr, "metrics", envOr("METRICS_ADDR", ":9090"),
		"address for metrics, health, and debug endpoints")
	flag.StringVar(&cfg.upstreamProxy, "upstream", envOr("UPSTREAM_PROXY", ""),
		"upstream proxy in host:port form (e.g. upstream.corp.local:8080)")
	flag.StringVar(&cfg.upstreamAuth, "upstream-auth", envOr("UPSTREAM_AUTH", ""),
		"Proxy-Authorization header value for upstream (e.g. 'Basic dXNlcjpwYXNz')")
	flag.StringVar(&cfg.certDir, "cert-dir", envOr("CERT_DIR", "/etc/proxy/certs"),
		"directory containing tls.crt, tls.key, ca.crt")
	flag.StringVar(&cfg.logLevel, "log-level", envOr("LOG_LEVEL", "info"),
		"log level: debug, info, warn, error")
	flag.BoolVar(&cfg.mtlsEnabled, "mtls", envOrBool("MTLS_ENABLED", true),
		"enable mTLS origination to upstream proxy")
	flag.BoolVar(&cfg.insecureSkipVerify, "insecure-skip-verify", envOrBool("INSECURE_SKIP_VERIFY", false),
		"skip TLS verification of upstream (testing only, NEVER production)")
	flag.DurationVar(&cfg.dialTimeout, "dial-timeout", 10*time.Second,
		"timeout for upstream dial")
	flag.DurationVar(&cfg.idleTimeout, "idle-timeout", 90*time.Second,
		"idle timeout for connections")
	flag.DurationVar(&cfg.readTimeout, "read-timeout", 60*time.Second,
		"HTTP read timeout")
	flag.DurationVar(&cfg.writeTimeout, "write-timeout", 60*time.Second,
		"HTTP write timeout")

	flag.Parse()

	if cfg.upstreamProxy == "" {
		slog.Error("UPSTREAM_PROXY / --upstream is required")
		os.Exit(2)
	}

	// Parse extra headers from env: EXTRA_HEADER_X_CORP_ID=value1 becomes X-Corp-Id: value1
	for _, e := range os.Environ() {
		if len(e) > len("EXTRA_HEADER_") && e[:len("EXTRA_HEADER_")] == "EXTRA_HEADER_" {
			rest := e[len("EXTRA_HEADER_"):]
			for i := 0; i < len(rest); i++ {
				if rest[i] == '=' {
					name := headerNameFromEnv(rest[:i])
					cfg.extraHeaders[name] = rest[i+1:]
					break
				}
			}
		}
	}

	return cfg
}

func headerNameFromEnv(s string) string {
	// Convert X_CORP_ID -> X-Corp-Id
	out := make([]byte, 0, len(s))
	upper := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' {
			out = append(out, '-')
			upper = true
			continue
		}
		if upper {
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			upper = false
		} else {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
		}
		out = append(out, c)
	}
	return string(out)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrBool(key string, def bool) bool {
	v := os.Getenv(key)
	switch v {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return def
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
