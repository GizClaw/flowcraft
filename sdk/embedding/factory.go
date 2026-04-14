package embedding

import (
	"fmt"
	"sort"
	"sync"
)

// ProviderFactory creates an Embedder for the given model.
// config contains connection settings (api_key, base_url, etc.).
type ProviderFactory func(model string, config map[string]any) (Embedder, error)

// ProviderRegistry is a thread-safe registry of embedding provider factories.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]ProviderFactory
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{providers: make(map[string]ProviderFactory)}
}

func (r *ProviderRegistry) Register(name string, factory ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = factory
}

// NewFromConfig creates an Embedder via the registered factory.
// Returns an error if the provider is not registered.
func (r *ProviderRegistry) NewFromConfig(provider, model string, config map[string]any) (Embedder, error) {
	r.mu.RLock()
	factory, ok := r.providers[provider]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("embedding: unknown provider %q", provider)
	}
	return factory(model, config)
}

func (r *ProviderRegistry) ListProviders() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// DefaultRegistry is the global provider registry used by init() auto-registration.
var DefaultRegistry = NewProviderRegistry()

// RegisterProvider registers a factory in DefaultRegistry.
func RegisterProvider(name string, factory ProviderFactory) {
	DefaultRegistry.Register(name, factory)
}

// NewFromConfig creates an Embedder from DefaultRegistry.
func NewFromConfig(provider, model string, config map[string]any) (Embedder, error) {
	return DefaultRegistry.NewFromConfig(provider, model, config)
}

// ListProviders lists providers from DefaultRegistry.
func ListProviders() []string {
	return DefaultRegistry.ListProviders()
}
