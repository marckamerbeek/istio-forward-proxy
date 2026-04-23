// Package certs beheert client certificaten voor mTLS origination naar de
// upstream proxy. Het leest tls.crt, tls.key en ca.crt uit een directory
// (doorgaans een gemount Kubernetes Secret) en laadt deze opnieuw wanneer
// cert-manager de certificaten roteert.
//
// Dit is het equivalent van DestinationRule met credentialName in Istio,
// maar dan native in Go zonder Envoy ertussen.
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

// Manager houdt de huidige TLS configuratie bij en watcht bestanden voor
// rotatie. Gebruik TLSConfig() om de meest recente config te krijgen.
type Manager struct {
	dir     string
	logger  *slog.Logger
	current atomic.Pointer[tls.Config]
}

// NewManager laadt de initiele certificaten en returned een manager. Als de
// certificaten niet leesbaar zijn returnt het een error zodat de pod niet
// opstart in een broken state.
func NewManager(dir string, logger *slog.Logger) (*Manager, error) {
	m := &Manager{dir: dir, logger: logger}
	if err := m.reload(); err != nil {
		return nil, fmt.Errorf("initial load: %w", err)
	}
	return m, nil
}

// TLSConfig returned de meest recent geladen TLS config. Deze is veilig om
// te delen over goroutines want we gebruiken atomic pointers.
func (m *Manager) TLSConfig() *tls.Config {
	return m.current.Load()
}

// reload leest alle bestanden opnieuw en atomair swapt de huidige config.
func (m *Manager) reload() error {
	certPath := filepath.Join(m.dir, "tls.crt")
	keyPath := filepath.Join(m.dir, "tls.key")
	caPath := filepath.Join(m.dir, "ca.crt")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load keypair: %w", err)
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

// Watch blokkeert tot ctx klaar is en reload certificaten bij elke wijziging.
// Kubernetes Secrets die als volume worden gemount gebruiken symlinks die
// atomair swappen; fsnotify vangt dit via de CREATE event op de parent dir.
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
			// Kubernetes Secret rotations trigger een CREATE event op de
			// ..data symlink. Ook WRITE/REMOVE events pakken we mee.
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
