package llm

import (
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ProviderFactory creates an LLM instance for the given model.
// config contains connection settings (api_key, base_url, etc.);
// each provider reads the keys it cares about and ignores the rest.
type ProviderFactory func(model string, config map[string]any) (LLM, error)

// ModelInfo describes a model offered by a provider.
type ModelInfo struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	Name     string `json:"name"`

	// Spec carries the model-fixed property set (caps + defaults +
	// limits).
	Spec ModelSpec `json:"spec,omitempty" yaml:"spec,omitempty"`

	// Deprecation, when non-zero, marks this catalog entry as a
	// legacy model the provider has scheduled (or already executed)
	// for retirement. The resolver still serves deprecated models —
	// removing them outright would silently break pinned callers —
	// but emits a one-shot telemetry warning per (provider, model)
	// the first time it resolves one, so dashboards / alerts can
	// surface the migration debt long before traffic breaks.
	//
	// See ModelDeprecation for field semantics.
	Deprecation ModelDeprecation `json:"deprecation,omitempty" yaml:"deprecation,omitempty"`
}

// ModelDeprecation is catalog metadata flagging a model as legacy.
// Any non-zero field activates the "model deprecated" path in the
// resolver; IsZero is the negation. Kept as a separate struct (not
// folded into ModelSpec) because deprecation is catalog metadata and
// must NOT be merged from per-deployment SpecOverride layers — only
// the catalog author can declare a model deprecated.
type ModelDeprecation struct {
	// RetiresAt marks the announced API shutdown date. Zero means
	// "deprecated, but no shutdown date announced yet". Use UTC
	// dates with day precision (HH:MM:SS portion is ignored by the
	// telemetry warning formatter).
	RetiresAt time.Time `json:"retires_at,omitempty" yaml:"retires_at,omitempty"`

	// Replacement names the recommended migration target as a fully
	// qualified "provider/model" string (e.g. "openai/gpt-5"). Empty
	// means "no specific replacement recommended".
	Replacement string `json:"replacement,omitempty" yaml:"replacement,omitempty"`

	// Notes is a free-form human-readable context attached to the
	// deprecation warning (e.g. a link to the provider's deprecation
	// announcement). Optional.
	Notes string `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// IsZero reports whether the deprecation carries no fields, i.e. the
// model is NOT deprecated. The resolver short-circuits on this.
func (d ModelDeprecation) IsZero() bool {
	return d.RetiresAt.IsZero() && d.Replacement == "" && d.Notes == ""
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

// NewFromConfig creates a raw LLM instance via the registered factory.
//
// Important: this method no longer wraps the instance with caps /
// defaults / limits middleware. All wrapping is the resolver's
// responsibility (one-shot assembly), so external callers using
// NewFromConfig directly get a bare provider connection.
//
// If you need the spec-wrapped form, go through DefaultResolver or
// apply WithDefaults / WithCaps / WithLimits manually.
func (r *ProviderRegistry) NewFromConfig(provider, model string, config map[string]any) (LLM, error) {
	r.mu.RLock()
	factory, ok := r.providers[provider]
	r.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFoundf("llm_provider %q not found", provider)
	}
	return factory(model, config)
}

// LookupModelSpec returns the catalog ModelSpec for a registered
// model, or a zero-value ModelSpec if the model is not found.
func (r *ProviderRegistry) LookupModelSpec(provider, model string) ModelSpec {
	if info, ok := r.LookupModel(provider, model); ok {
		return info.Spec
	}
	return ModelSpec{}
}

// LookupModel returns the full catalog ModelInfo for a registered
// (provider, model) pair, plus an `ok` flag. Used by the resolver to
// surface non-spec metadata (deprecation, etc.) without forcing
// callers through the older Spec-only path.
func (r *ProviderRegistry) LookupModel(provider, model string) (ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.providerModels[provider] {
		if m.Name == model {
			return m, true
		}
	}
	return ModelInfo{}, false
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
