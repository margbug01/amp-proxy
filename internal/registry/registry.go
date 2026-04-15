// Package registry provides a minimal in-memory model-provider registry.
//
// In amp-proxy the registry is always empty in production: every request is
// forwarded to the Amp upstream, so no local providers ever register. The
// implementation is kept so that inherited amp tests that call
// RegisterClient / UnregisterClient continue to compile and run. Once those
// tests are either adapted or removed, this whole package can be deleted.
//
// Derived in spirit from github.com/router-for-me/CLIProxyAPI/v6/internal/registry
// (MIT), but implemented from scratch as a small subset: only the surface
// referenced by the amp module and its tests is present.
package registry

import "sync"

// ModelInfo describes a single model known to a client.
type ModelInfo struct {
	ID      string
	OwnedBy string
	Type    string
}

// Registry is a thread-safe in-memory table of client → (provider, models).
type Registry struct {
	mu      sync.RWMutex
	clients map[string]clientEntry
}

type clientEntry struct {
	provider string
	models   []*ModelInfo
}

var globalRegistry = &Registry{clients: make(map[string]clientEntry)}

// GetGlobalRegistry returns the process-wide registry singleton.
func GetGlobalRegistry() *Registry {
	return globalRegistry
}

// RegisterClient records that the named client exposes the given models via
// the specified provider. Registering the same client twice replaces the
// previous entry.
func (r *Registry) RegisterClient(name, provider string, models []*ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[name] = clientEntry{provider: provider, models: models}
}

// UnregisterClient removes any entry for the named client.
func (r *Registry) UnregisterClient(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, name)
}

// GetModelProviders returns the distinct provider names that have a model
// registered under the given ID.
func (r *Registry) GetModelProviders(modelName string) []string {
	if modelName == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := make(map[string]struct{})
	providers := make([]string, 0, len(r.clients))
	for _, entry := range r.clients {
		for _, m := range entry.models {
			if m == nil || m.ID != modelName {
				continue
			}
			if _, ok := seen[entry.provider]; ok {
				continue
			}
			seen[entry.provider] = struct{}{}
			providers = append(providers, entry.provider)
		}
	}
	return providers
}
