package llm

import (
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ProviderFactory creates an LLM instance for the given model.
// config contains connection settings (api_key, base_url, etc.);
// each provider reads the keys it cares about and ignores the rest.
type ProviderFactory func(model string, config map[string]any) (LLM, error)

// Capability identifies a feature that can be disabled for a model.
type Capability string

const (
	CapTemperature Capability = "temperature"
	CapJSONSchema  Capability = "json_schema"
	CapJSONMode    Capability = "json_mode"
)

// ModelCaps declares which capabilities a model does not support.
// Zero-value means all capabilities are supported.
// Use Supports(cap) instead of inspecting Disabled directly.
type ModelCaps struct {
	Disabled map[Capability]bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
}

// Supports reports whether the model supports the given capability.
func (c ModelCaps) Supports(cap Capability) bool {
	return !c.Disabled[cap]
}

// IsZero reports whether no capabilities are disabled.
func (c ModelCaps) IsZero() bool {
	return len(c.Disabled) == 0
}

// DisabledCaps creates a ModelCaps with the given capabilities disabled.
func DisabledCaps(caps ...Capability) ModelCaps {
	m := ModelCaps{Disabled: make(map[Capability]bool, len(caps))}
	for _, c := range caps {
		m.Disabled[c] = true
	}
	return m
}

// ModelInfo describes a model offered by a provider.
type ModelInfo struct {
	Provider string    `json:"provider"`
	Label    string    `json:"label"`
	Name     string    `json:"name"`
	Caps     ModelCaps `json:"caps,omitempty"`
}

// ProviderRegistry is a thread-safe registry of LLM provider factories
// and their supported models.
type ProviderRegistry struct {
	mu             sync.RWMutex
	providers      map[string]ProviderFactory
	providerModels map[string][]ModelInfo
}

// NewProviderRegistry returns an empty registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers:      make(map[string]ProviderFactory),
		providerModels: make(map[string][]ModelInfo),
	}
}

// Register adds a provider factory. Overwrites any previous registration.
func (r *ProviderRegistry) Register(name string, factory ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = factory
}

// NewFromConfig creates an LLM instance via the registered factory and
// wraps it with CapsMiddleware seeded from the registry catalog.
//
// Expected config structure:
//
//	{
//	  "api_key": "...",
//	  "base_url": "...",
//	}
//
// Caps must come through the typed pipeline (ProviderConfig.Caps /
// ModelConfig.Caps when going through DefaultResolver); the untyped
// config["caps"] sub-object is no longer read.
func (r *ProviderRegistry) NewFromConfig(provider, model string, config map[string]any) (LLM, error) {
	r.mu.RLock()
	factory, ok := r.providers[provider]
	registryCaps := r.lookupModelCaps(provider, model)
	r.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFoundf("llm_provider %q not found", provider)
	}
	inst, err := factory(model, config)
	if err != nil {
		return nil, err
	}
	return CapsMiddleware(inst, registryCaps), nil
}

// LookupModelCaps returns the ModelCaps for a registered model.
// Returns zero-value ModelCaps if the model is not found.
func (r *ProviderRegistry) LookupModelCaps(provider, model string) ModelCaps {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lookupModelCaps(provider, model)
}

func (r *ProviderRegistry) lookupModelCaps(provider, model string) ModelCaps {
	for _, m := range r.providerModels[provider] {
		if m.Name == model {
			return m.Caps
		}
	}
	return ModelCaps{}
}

// ListProviders returns sorted provider names.
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

// RegisterModels associates a list of models with a provider.
func (r *ProviderRegistry) RegisterModels(provider string, models []ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]ModelInfo, len(models))
	copy(cp, models)
	for i := range cp {
		cp[i].Provider = provider
	}
	r.providerModels[provider] = cp
}

// ListAllModels returns all registered models across all providers.
func (r *ProviderRegistry) ListAllModels() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var all []ModelInfo
	for _, ms := range r.providerModels {
		all = append(all, ms...)
	}
	return all
}

// --- DefaultRegistry + package-level convenience functions ---

// DefaultRegistry is the global provider registry used by init() auto-registration.
var DefaultRegistry = NewProviderRegistry()

// RegisterProvider registers a factory in DefaultRegistry.
func RegisterProvider(name string, factory ProviderFactory) {
	DefaultRegistry.Register(name, factory)
}

// NewFromConfig creates an LLM from DefaultRegistry.
func NewFromConfig(provider, model string, config map[string]any) (LLM, error) {
	return DefaultRegistry.NewFromConfig(provider, model, config)
}

// ListProviders lists providers from DefaultRegistry.
func ListProviders() []string {
	return DefaultRegistry.ListProviders()
}

// RegisterProviderModels registers model info in DefaultRegistry.
func RegisterProviderModels(provider string, models []ModelInfo) {
	DefaultRegistry.RegisterModels(provider, models)
}

// ListAllModels returns all models from DefaultRegistry.
func ListAllModels() []ModelInfo {
	return DefaultRegistry.ListAllModels()
}
