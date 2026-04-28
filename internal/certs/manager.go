// Package certs manages client certificates for mTLS origination to the
// upstream proxy. It reads tls.crt, tls.key, and ca.crt from a directory
// (typically a mounted Kubernetes Secret) and hot-reloads them when
// cert-manager rotates the certificates.
//
// This is the Go-native equivalent of a DestinationRule with credentialName
// in Istio, without Envoy in the path.
package certs

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	"context"

	"github.com/fsnotify/fsnotify"
)

// Manager holds the current TLS configuration and watches for certificate
// rotation. Call TLSConfig() to get the most recent config.
type Manager struct {
	dir     string
	logger  *slog.Logger
	current atomic.Pointer[tls.Config]
}

// NewManager loads the initial certificates and returns a manager. Returns an
// error if the certificates cannot be read, preventing startup in a broken state.
func NewManager(dir string, logger *slog.Logger) (*Manager, error) {
	m := &Manager{dir: dir, logger: logger}
	if err := m.reload(); err != nil {
		return nil, fmt.Errorf("initial load: %w", err)
	}
	return m, nil
}

// TLSConfig returns the most recently loaded TLS config. Safe for concurrent
// use via atomic pointer.
func (m *Manager) TLSConfig() *tls.Config {
	return m.current.Load()
}

// reload reads all certificate files and atomically swaps the current config.
func (m *Manager) reload() error {
	certPath := filepath.Join(m.dir, "tls.crt")
	keyPath := filepath.Join(m.dir, "tls.key")
	caPath := filepath.Join(m.dir, "ca.crt")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load keypair: %w", err)
	}
	// tls.LoadX509KeyPair does not populate Leaf; parse it explicitly so the
	// logging below and any TLS handshake inspection can access cert metadata.
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse leaf certificate: %w", err)
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("read CA: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("no CA certificates found in %s", caPath)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}
	m.current.Store(cfg)
	m.logger.Info("loaded client certificates",
		"cert_dir", m.dir,
		"cert_subject", cert.Leaf.Subject.String(),
		"cert_not_after", cert.Leaf.NotAfter,
	)
	return nil
}

// Watch blocks until ctx is done, reloading certificates on every file change.
// Kubernetes Secrets mounted as volumes use symlinks that swap atomically;
// fsnotify catches this via CREATE events on the parent directory.
func (m *Manager) Watch(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		m.logger.Error("failed to create fsnotify watcher", "error", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(m.dir); err != nil {
		m.logger.Error("failed to watch cert dir", "dir", m.dir, "error", err)
		return
	}

	m.logger.Info("watching certificate directory", "dir", m.dir)

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Kubernetes Secret rotations trigger a CREATE event on the
			// ..data symlink. WRITE/REMOVE/RENAME events are also handled.
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				m.logger.Debug("cert file event", "event", event.String())
				if err := m.reload(); err != nil {
					m.logger.Error("failed to reload certificates", "error", err)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			m.logger.Error("fsnotify error", "error", err)
		}
	}
}
