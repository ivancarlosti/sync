package providers

import (
	"fmt"
	"sort"
	"sync"

	"github.com/ivancarlosti/sync/internal/models"
)

// Registry indexes the available providers by name. It is built once at boot
// and is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	items map[models.ProviderName]Provider
}

// NewRegistry builds a registry from the given providers. A duplicate name
// panics: it can only be a programming error.
func NewRegistry(list ...Provider) *Registry {
	registry := &Registry{items: make(map[models.ProviderName]Provider, len(list))}
	for _, provider := range list {
		if provider == nil {
			continue
		}
		name := provider.Name()
		if _, exists := registry.items[name]; exists {
			panic(fmt.Sprintf("providers: duplicate registration for %q", name))
		}
		registry.items[name] = provider
	}
	return registry
}

// Get returns the provider registered under name.
func (r *Registry) Get(name models.ProviderName) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.items[name]
	if !ok {
		return nil, fmt.Errorf("providers: %q is not available in this build", name)
	}
	return provider, nil
}

// Names returns the registered provider names, sorted for stable output.
func (r *Registry) Names() []models.ProviderName {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]models.ProviderName, 0, len(r.items))
	for name := range r.items {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// SiteBrowserFor returns the SiteBrowser implementation of a provider, if any.
func (r *Registry) SiteBrowserFor(name models.ProviderName) (SiteBrowser, bool) {
	provider, err := r.Get(name)
	if err != nil {
		return nil, false
	}
	browser, ok := provider.(SiteBrowser)
	return browser, ok
}
