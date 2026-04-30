package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"golang.org/x/sync/singleflight"
)

var (
	inferMu    sync.RWMutex
	inferRules []InferRule
)

func init() {
	defaultRules := []InferRule{
		{"gpt-", "openai"},
		{"o1", "openai"},
		{"o3", "openai"},
		{"claude-", "anthropic"},
		{"deepseek-", "deepseek"},
		{"qwen-", "qwen"},
		{"doubao-", "bytedance"},
		{"abab", "minimax"},
		{"llama", "ollama"},
		{"mistral", "ollama"},
		{"gemma", "ollama"},
	}
	inferRules = defaultRules
}

// ProviderConfig holds provider-level configuration for LLM resolution.
//
// A single provider (e.g. "openai") can have multiple ProviderConfig
// entries distinguished by Profile, supporting multi-tenant
// deployments, key-pool spreading, and prod/staging splits.
//
// Config carries provider connection settings (api_key, base_url, ...)
// and is forwarded as-is to ProviderFactory. SpecOverride layers on
// top of the catalog ModelSpec at resolve time (see mergeSpec for
// the field-wise merge rules).
type ProviderConfig struct {
	Provider string `json:"provider"`

	// Profile identifies a credential profile within a provider.
	// Empty string is the default profile, equivalent to today's
	// single-credential semantics. See doc/sdk-llm-redesign.md §3.4
	// for usage scenarios.
	Profile string `json:"profile,omitempty" yaml:"profile,omitempty"`

	// Config is forwarded as-is to ProviderFactory.
	Config map[string]any `json:"config"`

	// SpecOverride layers on top of the catalog ModelSpec. Field-wise
	// merge: zero values in SpecOverride do not mask catalog values;
	// non-zero values win. Limits use stricter-wins semantics
	// (see mergeLimits) to prevent loosening catalog declarations.
	SpecOverride ModelSpec `json:"spec,omitempty" yaml:"spec,omitempty"`
}

// ModelConfig holds user-supplied per-model overrides that layer on
// top of a ProviderConfig at resolve time. Returned by ModelConfigStore.
type ModelConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// SpecOverride layers on top of (catalog ⊕ ProviderConfig.SpecOverride)
	// using the same field-wise merge as ProviderConfig.SpecOverride.
	SpecOverride ModelSpec `json:"spec,omitempty" yaml:"spec,omitempty"`

	// Extra is shallow-merged into ProviderConfig.Config (per-model
	// values overwrite provider values). Useful for routing one
	// model through a different base_url while sibling models keep
	// the default.
	Extra map[string]any `json:"extra,omitempty"`
}

// DefaultModelRef points at the resolver's current default model. Used
// when callers invoke Resolve("") without a model identifier.
type DefaultModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ProviderConfigStore is the only required store interface. It looks
// up provider-level configuration by (provider, profile) tuple.
//
// Lookup contract: exact match on (provider, profile). When the
// caller did not set a credential profile, profile is "" — the store
// returns the default-profile entry. When the caller set a specific
// profile, the store returns the matching entry or
// errdefs.NotFound — silently falling back to the default profile is
// FORBIDDEN (it would silently route a tenant-scoped call to the
// wrong credential, a billing / safety incident waiting to happen).
// Stores wanting fallback semantics must implement them explicitly.
type ProviderConfigStore interface {
	GetProviderConfig(ctx context.Context, provider, profile string) (*ProviderConfig, error)
}

// ModelConfigStore is an optional extension. When the store passed to
// DefaultResolver also implements it, the resolver will fetch and
// merge per-model overrides on top of the provider config before
// instantiating an LLM. Returning an errdefs.NotFound error is
// treated as "no overrides" and is silently ignored; any other error
// fails Resolve.
type ModelConfigStore interface {
	GetModelConfig(ctx context.Context, provider, model string) (*ModelConfig, error)
}

// DefaultModelStore is an optional extension. When the store passed to
// DefaultResolver also implements it, Resolve("") consults it before
// falling back to WithFallbackModel. Returning errdefs.NotFound is
// treated as "no default set"; any other error fails Resolve.
type DefaultModelStore interface {
	GetDefaultModel(ctx context.Context) (*DefaultModelRef, error)
}

// LLMResolver resolves a model string (e.g. "openai/gpt-4o") into an
// LLM instance, with a profile-aware cache and selective
// invalidation.
type LLMResolver interface {
	Resolve(ctx context.Context, model string) (LLM, error)

	// InvalidateCache removes cached LLM instances. With no options it
	// clears every entry; options narrow the scope. See
	// WithProvider / WithModel / WithProfile.
	InvalidateCache(opts ...InvalidateOption)
}

// ResolverOption configures a defaultResolver.
type ResolverOption func(*defaultResolver)

// WithFallbackModel sets the default model used when Resolve("") is
// called and no DefaultModelStore yields a result.
func WithFallbackModel(model string) ResolverOption {
	return func(r *defaultResolver) { r.fallback = model }
}

// WithPolicyCaps merges resolver-wide policy caps into every LLM the
// resolver produces. Distinct from a model's own ModelCaps in intent:
// these are runtime-enforced policy switches (e.g. a debug flag that
// disables JSON mode globally, a feature-gate that hides streaming
// across the fleet). OR-merged with catalog and config caps at
// resolve time — any layer disabling a cap wins.
func WithPolicyCaps(caps ModelCaps) ResolverOption {
	return func(r *defaultResolver) { r.policyCaps = mergeCaps(r.policyCaps, caps) }
}

// DefaultResolver creates an LLMResolver backed by the DefaultRegistry
// and the given store. The store must implement ProviderConfigStore;
// it may additionally implement ModelConfigStore (for per-model
// overrides) and DefaultModelStore (for Resolve("") routing).
func DefaultResolver(store ProviderConfigStore, opts ...ResolverOption) LLMResolver {
	r := &defaultResolver{
		registry: DefaultRegistry,
		store:    store,
		cache:    make(map[cacheKey]LLM),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

type defaultResolver struct {
	registry   *ProviderRegistry
	store      ProviderConfigStore
	fallback   string
	policyCaps ModelCaps

	mu    sync.RWMutex
	cache map[cacheKey]LLM
	sf    singleflight.Group
}

// cacheKey is the three-dimensional resolver cache key. Using a struct
// (rather than a "/-or-#-separated" string) avoids collision /
// ambiguity in the unlikely case any of provider / model / profile
// contains a delimiter character.
type cacheKey struct {
	provider string
	model    string
	profile  string
}

func (k cacheKey) String() string {
	if k.profile == "" {
		return k.provider + "/" + k.model
	}
	return k.provider + "/" + k.model + "#" + k.profile
}

// Resolve maps a "provider/model" string to an LLM instance. Empty
// modelStr triggers default-model lookup in this order:
//
//  1. DefaultModelStore.GetDefaultModel (preferred)
//  2. WithFallbackModel
//
// Per-call credential profile is read from the context via
// CredentialProfileFromContext; "" means the default profile.
func (r *defaultResolver) Resolve(ctx context.Context, modelStr string) (LLM, error) {
	if modelStr == "" {
		modelStr = r.resolveDefaultModel(ctx)
	}
	if modelStr == "" {
		return nil, errdefs.Validationf("no model specified and no fallback configured")
	}

	profile := CredentialProfileFromContext(ctx)
	provider, modelName := parseModelString(modelStr)
	key := cacheKey{provider: provider, model: modelName, profile: profile}

	r.mu.RLock()
	if cached, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	r.mu.RUnlock()

	v, err, _ := r.sf.Do(key.String(), func() (any, error) {
		return r.createLLM(ctx, key)
	})
	if err != nil {
		return nil, err
	}
	return v.(LLM), nil
}

// resolveDefaultModel returns the default model identifier or "".
// Tries DefaultModelStore first, then WithFallbackModel.
func (r *defaultResolver) resolveDefaultModel(ctx context.Context) string {
	if r.store != nil {
		if dms, ok := r.store.(DefaultModelStore); ok {
			if ref, err := dms.GetDefaultModel(ctx); err == nil && ref != nil &&
				ref.Provider != "" && ref.Model != "" {
				return ref.Provider + "/" + ref.Model
			}
		}
	}
	return r.fallback
}

// createLLM does the one-shot assembly: resolve config, build the
// raw provider, then wrap with the spec-driven middleware stack. No
// unwrapping / re-wrapping — every layer is applied exactly once.
func (r *defaultResolver) createLLM(ctx context.Context, key cacheKey) (LLM, error) {
	pc, err := r.store.GetProviderConfig(ctx, key.provider, key.profile)
	if err != nil {
		return nil, fmt.Errorf("resolve provider config for %s: %w", key, err)
	}

	// Per-model overrides are optional. NotFound is silent; any other
	// store error fails resolve so we don't hide misconfiguration.
	var mc *ModelConfig
	if mcs, ok := r.store.(ModelConfigStore); ok {
		got, mErr := mcs.GetModelConfig(ctx, key.provider, key.model)
		if mErr == nil {
			mc = got
		} else if !errdefs.IsNotFound(mErr) {
			return nil, fmt.Errorf("resolve model config for %s: %w", key, mErr)
		}
	}

	// Shallow merge: per-model Extra overwrites provider Config keys.
	// Predictable, matches the user mental model of "the value I set
	// wins as-is" — never deep-merged.
	connConfig := pc.Config
	if mc != nil && len(mc.Extra) > 0 {
		connConfig = shallowMergeConfig(pc.Config, mc.Extra)
	}

	inst, err := r.registry.NewFromConfig(key.provider, key.model, connConfig)
	if err != nil {
		return nil, err
	}

	// Surface catalog deprecation as a one-shot telemetry warning
	// per (provider, model). Fires AFTER the factory call so we
	// don't spam warnings for misconfigured providers that fail at
	// instantiation.
	if info, ok := r.registry.LookupModel(key.provider, key.model); ok && !info.Deprecation.IsZero() {
		warnDeprecatedModel(ctx, key.provider, key.model, info.Deprecation)
	}

	// Spec merge order — later layers override earlier non-zero fields,
	// with caps OR-merged and limits stricter-winning.
	specLayers := []ModelSpec{
		r.registry.LookupModelSpec(key.provider, key.model), // catalog
		pc.SpecOverride, // provider override
	}
	if mc != nil {
		specLayers = append(specLayers, mc.SpecOverride) // per-model override
	}
	spec := mergeSpec(specLayers...)
	// Resolver-wide policy caps are the LAST OR layer — they always
	// have the final say across every (provider, model, profile).
	spec.Caps = mergeCaps(spec.Caps, r.policyCaps)

	// Wrap order is innermost → outermost so per-call invocation runs
	// defaults → caps → limits (see RFC §3.6 for rationale):
	//
	//   inst        ← raw provider call
	//   WithLimits  ← clamp numeric fields (innermost)
	//   WithCaps    ← drop / downgrade unsupported fields, validate msgs
	//   WithDefaults← fill nil fields from defaults (outermost)
	inst = WithLimits(inst, spec.Limits)
	inst = WithCaps(inst, spec.Caps)
	inst = WithDefaults(inst, spec.Defaults)

	r.mu.Lock()
	r.cache[key] = inst
	r.mu.Unlock()

	return inst, nil
}

// shallowMergeConfig returns base ⊕ overlay where overlay's keys win.
// Both inputs are left untouched. Returns base verbatim if overlay is
// empty (avoids needless allocation).
func shallowMergeConfig(base, overlay map[string]any) map[string]any {
	if len(overlay) == 0 {
		return base
	}
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// InvalidateOption narrows the scope of InvalidateCache. With no
// options the entire cache is cleared. Options compose with AND
// semantics: only entries matching every supplied option are removed.
type InvalidateOption func(*invalidateFilter)

type invalidateFilter struct {
	provider    string
	model       string
	profile     string
	hasProvider bool
	hasModel    bool
	hasProfile  bool
}

// WithProvider scopes invalidation to one provider.
func WithProvider(p string) InvalidateOption {
	return func(f *invalidateFilter) { f.provider = p; f.hasProvider = true }
}

// WithModel scopes invalidation to one model name (regardless of
// provider unless WithProvider is also supplied).
func WithModel(m string) InvalidateOption {
	return func(f *invalidateFilter) { f.model = m; f.hasModel = true }
}

// WithProfile scopes invalidation to one credential profile. Useful
// for evicting a single tenant's cached LLM instances after rotating
// their key.
func WithProfile(p string) InvalidateOption {
	return func(f *invalidateFilter) { f.profile = p; f.hasProfile = true }
}

func (f *invalidateFilter) matches(k cacheKey) bool {
	if f.hasProvider && k.provider != f.provider {
		return false
	}
	if f.hasModel && k.model != f.model {
		return false
	}
	if f.hasProfile && k.profile != f.profile {
		return false
	}
	return true
}

// InvalidateCache removes cached LLM instances matching every supplied
// option. With no options the whole cache is cleared.
func (r *defaultResolver) InvalidateCache(opts ...InvalidateOption) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(opts) == 0 {
		r.cache = make(map[cacheKey]LLM)
		return
	}
	f := &invalidateFilter{}
	for _, o := range opts {
		o(f)
	}
	for k := range r.cache {
		if f.matches(k) {
			delete(r.cache, k)
		}
	}
}

// parseModelString splits "provider/model" into (provider, model).
// If there is no "/" separator, the whole string is treated as the model
// name and the provider is inferred via the global InferRule table.
func parseModelString(s string) (provider, modelName string) {
	if idx := strings.Index(s, "/"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return inferProvider(s), s
}

// InferRule maps a model name prefix to a provider name.
type InferRule struct {
	Prefix   string
	Provider string
}

// RegisterInferRule adds a model-prefix → provider mapping used when
// Resolve receives a bare model name without a "provider/" prefix.
// Rules registered later take priority (checked first).
func RegisterInferRule(prefix, provider string) {
	inferMu.Lock()
	defer inferMu.Unlock()
	inferRules = append([]InferRule{{Prefix: prefix, Provider: provider}}, inferRules...)
}

func inferProvider(model string) string {
	inferMu.RLock()
	defer inferMu.RUnlock()
	for _, r := range inferRules {
		if strings.HasPrefix(model, r.Prefix) {
			return r.Provider
		}
	}
	return model
}
