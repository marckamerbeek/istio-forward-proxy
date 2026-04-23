// Package serviceentry implements a Kubernetes API watcher for
// networking.istio.io/v1 ServiceEntries. Each ServiceEntry in the cluster
// defines which external hosts the forward proxy allows. This is the
// equivalent of Istio's REGISTRY_ONLY outboundTrafficPolicy combined with
// ServiceEntry resources.
//
// The watcher uses the Istio client-go library to maintain an in-memory
// allowlist that the proxy handler queries via AllowHost().
package serviceentry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	istioinformers "istio.io/client-go/pkg/informers/externalversions"
	istioclient "istio.io/client-go/pkg/clientset/versioned"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Watcher maintains the current allowlist. All methods are safe for concurrent
// use via RWMutex and an atomic synced flag.
type Watcher struct {
	client  istioclient.Interface
	factory istioinformers.SharedInformerFactory
	logger  *slog.Logger

	mu        sync.RWMutex
	hosts     map[string]hostEntry // key: exact hostname
	wildcards []wildcardEntry      // e.g. *.example.com

	synced atomic.Bool
}

type hostEntry struct {
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	Ports        []uint32 `json:"ports"`
	AllowedPorts []uint32 `json:"allowed_ports"`
}

type wildcardEntry struct {
	Suffix string    // .example.com (including leading dot)
	Entry  hostEntry `json:"entry"`
}

// NewWatcher creates a watcher using in-cluster config when running inside a
// pod, or kubeconfig when the KUBECONFIG env var is set.
func NewWatcher(logger *slog.Logger) (*Watcher, error) {
	cfg, err := loadKubeConfig()
	if err != nil {
		return nil, err
	}

	cli, err := istioclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	factory := istioinformers.NewSharedInformerFactory(cli, 10*time.Minute)

	w := &Watcher{
		client:  cli,
		factory: factory,
		logger:  logger,
		hosts:   make(map[string]hostEntry),
	}

	return w, nil
}

func loadKubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// Run starts the informer and blocks until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	informer := w.factory.Networking().V1().ServiceEntries().Informer()

	_, err := informer.AddEventHandler(watchHandlers(w))
	if err != nil {
		w.logger.Error("failed to add event handler", "error", err)
		return
	}

	w.factory.Start(ctx.Done())

	w.logger.Info("waiting for ServiceEntry cache sync")
	w.factory.WaitForCacheSync(ctx.Done())
	w.synced.Store(true)
	w.logger.Info("ServiceEntry cache synced", "count", w.Count())

	<-ctx.Done()
}

// Synced returns true once the initial list from the API server is complete.
func (w *Watcher) Synced() bool {
	return w.synced.Load()
}

// Count returns the number of unique allowed hosts.
func (w *Watcher) Count() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.hosts) + len(w.wildcards)
}

// AllowHost reports whether a host:port combination is allowed by the proxy.
// Also returns the matching hostEntry for logging/debug purposes.
//
// Matching rules:
//  1. Exact match on host — allowed if the port also matches (or no ports
//     are specified in the ServiceEntry, meaning all ports are allowed).
//  2. Wildcard match on *.example.com — allowed if the host ends with
//     .example.com and the port matches.
func (w *Watcher) AllowHost(host string, port uint32) (bool, hostEntry) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	w.mu.RLock()
	defer w.mu.RUnlock()

	if entry, ok := w.hosts[host]; ok {
		if portAllowed(entry, port) {
			return true, entry
		}
		return false, entry
	}

	for _, wc := range w.wildcards {
		if strings.HasSuffix(host, wc.Suffix) {
			if portAllowed(wc.Entry, port) {
				return true, wc.Entry
			}
			return false, wc.Entry
		}
	}

	return false, hostEntry{}
}

func portAllowed(e hostEntry, port uint32) bool {
	if len(e.AllowedPorts) == 0 {
		return true // no port restriction means all ports are allowed
	}
	for _, p := range e.AllowedPorts {
		if p == port {
			return true
		}
	}
	return false
}

// DumpJSON writes the current allowlist as JSON for the debug endpoint.
func (w *Watcher) DumpJSON(out io.Writer) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	payload := struct {
		Synced    bool                 `json:"synced"`
		Hosts     map[string]hostEntry `json:"hosts"`
		Wildcards []wildcardEntry      `json:"wildcards"`
	}{
		Synced:    w.synced.Load(),
		Hosts:     w.hosts,
		Wildcards: w.wildcards,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

// rebuild recomputes the allowlist from the informer cache on every event.
// A full rebuild is simpler and more robust than incremental updates because
// ServiceEntry changes are infrequent.
func (w *Watcher) rebuild() {
	lister := w.factory.Networking().V1().ServiceEntries().Lister()
	entries, err := lister.List(labelsEverything())
	if err != nil {
		w.logger.Error("failed to list ServiceEntries", "error", err)
		return
	}

	hosts := make(map[string]hostEntry)
	var wildcards []wildcardEntry

	for _, se := range entries {
		var ports []uint32
		for _, p := range se.Spec.Ports {
			ports = append(ports, p.Number)
		}

		for _, h := range se.Spec.Hosts {
			h = strings.ToLower(strings.TrimSuffix(h, "."))
			entry := hostEntry{
				Namespace:    se.Namespace,
				Name:         se.Name,
				Ports:        ports,
				AllowedPorts: ports,
			}

			if strings.HasPrefix(h, "*.") {
				wildcards = append(wildcards, wildcardEntry{
					Suffix: h[1:], // ".example.com"
					Entry:  entry,
				})
				continue
			}
			hosts[h] = entry
		}
	}

	w.mu.Lock()
	w.hosts = hosts
	w.wildcards = wildcards
	w.mu.Unlock()

	w.logger.Debug("allowlist rebuilt",
		"exact_hosts", len(hosts),
		"wildcards", len(wildcards),
	)
}
