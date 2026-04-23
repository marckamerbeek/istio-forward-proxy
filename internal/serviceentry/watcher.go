// Package serviceentry implementeert een Kubernetes API watcher op
// networking.istio.io/v1 ServiceEntries. Elke ServiceEntry in het cluster
// bepaalt welke externe hosts door de forward proxy heen mogen. Dit is
// het equivalent van REGISTRY_ONLY outboundTrafficPolicy combinatie met
// ServiceEntry in Istio.
//
// De watcher gebruikt de Istio client-go library en maakt een in-memory
// allowlist die via AllowHost() wordt bevraagd door de proxy handler.
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

// Watcher houdt de actuele allowlist bij. Methods zijn safe voor concurrent
// use via RWMutex en atomic synced flag.
type Watcher struct {
	client     istioclient.Interface
	factory    istioinformers.SharedInformerFactory
	logger     *slog.Logger

	mu        sync.RWMutex
	hosts     map[string]hostEntry // key: exacte hostname, value: metadata
	wildcards []wildcardEntry      // wildcards zoals *.example.com

	synced atomic.Bool
}

type hostEntry struct {
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	Ports        []uint32 `json:"ports"`
	AllowedPorts []uint32 `json:"allowed_ports"`
}

type wildcardEntry struct {
	Suffix string    // .example.com (incl. leading dot)
	Entry  hostEntry `json:"entry"`
}

// NewWatcher maakt een watcher die gebruik maakt van de in-cluster config
// als het in een pod draait, of kubeconfig als KUBECONFIG env var is gezet.
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
	// Probeer eerst in-cluster config
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	// Val terug op kubeconfig file
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// Run start de informer en blokkeert tot ctx klaar is.
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

// Synced returnt true zodra de initial list klaar is.
func (w *Watcher) Synced() bool {
	return w.synced.Load()
}

// Count returnt het aantal unieke hosts.
func (w *Watcher) Count() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.hosts) + len(w.wildcards)
}

// AllowHost bepaalt of een host:port combinatie door de proxy mag.
// Geeft ook een hostEntry terug voor logging/debug doeleinden.
//
// Matching regels:
//  1. Exacte match op host — toegestaan als poort ook matcht (of geen
//     poorten gespecificeerd in ServiceEntry, dan alle poorten toe)
//  2. Wildcard match op *.example.com — toegestaan als host eindigt op
//     .example.com en poort matcht
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
		return true // geen port restrictie = alle poorten
	}
	for _, p := range e.AllowedPorts {
		if p == port {
			return true
		}
	}
	return false
}

// DumpJSON schrijft de huidige allowlist als JSON (voor debug endpoint).
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

// rebuild wordt bij elke event aangeroepen om de allowlist te herberekenen
// vanuit de informer cache. Dit is simpeler en robuuster dan incrementele
// updates want ServiceEntry wijzigingen zijn zeldzaam.
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
				AllowedPorts: ports, // huidig: alle poorten uit ServiceEntry toestaan
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
