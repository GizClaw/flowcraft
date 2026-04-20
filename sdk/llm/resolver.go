package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"golang.org/x/sync/singleflight"
)

// ProviderConfig holds provider-level configuration for LLM resolution.
//
// Config carries provider connection settings (api_key, base_url, ...)
// and is forwarded as-is to ProviderFactory. Caps is a strongly-typed
// per-provider capability override (applied to every model under this
// provider). For per-model overrides, implement ModelConfigStore.
type ProviderConfig struct {
	Provider string         `json:"provider"`
	Config   map[string]any `json:"config"`
	// Caps disables capabilities for every model of this provider.
	// Merged (OR) with registry caps and ModelConfig.Caps at resolve time.
	Caps ModelCaps `json:"caps,omitempty"`
}

// ModelConfig holds user-supplied per-model overrides that layer on top
// of a ProviderConfig at resolve time. Returned by ModelConfigStore.
//
// Caps merges (OR) with ProviderConfig.Caps and the registry caps —
// any layer disabling a capability wins. Extra is shallow-merged into
// ProviderConfig.Config (per-model values overwrite provider values),
// useful for routing one model through a different base_url while
// other models of the same provider keep the default.
type ModelConfig struct {
	Provider string         `json:"provider"`
	Model    string         `json:"model"`
	Caps     ModelCaps      `json:"caps,omitempty"`
	Extra    map[string]any `json:"extra,omitempty"`
}

// DefaultModelRef points at the resolver's current default model. Used
// when callers invoke Resolve("") without a model identifier.
type DefaultModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ProviderConfigStore is the only required store interface. It looks
// up provider-level configuration by provider name.
type ProviderConfigStore interface {
	GetProviderConfig(ctx context.Context, provider string) (*ProviderConfig, error)
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

// LLMResolver resolves a model string (e.g. "openai/gpt-4o") into an LLM
// instance.
type LLMResolver interface {
	Resolve(ctx context.Context, model string) (LLM, error)
	InvalidateCache(provider string)
}

// GlobalDefaultProvider is the legacy well-known key for the default
// model pointer. Kept for backward compatibility with stores that
// stash {provider, model} into a ProviderConfig row keyed by this
// constant.
//
// Deprecated: implement DefaultModelStore on your store instead.
// Removed in v0.2.0.
const GlobalDefaultProvider = "__global_default__"

// ResolverOption configures a defaultResolver.
type ResolverOption func(*defaultResolver)

// WithFallbackModel sets the default model used when Resolve("") is
// called and no DefaultModelStore (or legacy GlobalDefaultProvider row)
// yields a result.
func WithFallbackModel(model string) ResolverOption {
	return func(r *defaultResolver) { r.fallback = model }
}

// WithExtraCaps merges additional ModelCaps into every LLM the resolver
// produces. Useful for runtime-wide policy (e.g. a debug switch that
// disables JSON mode globally). Combined (OR) with registry, provider
// and per-model caps at resolve time.
func WithExtraCaps(caps ModelCaps) ResolverOption {
	return func(r *defaultResolver) { r.extraCaps = mergeCaps(r.extraCaps, caps) }
}

// WithModelCaps is the legacy spelling of WithExtraCaps.
//
// Deprecated: use WithExtraCaps. Removed in v0.2.0.
func WithModelCaps(caps ModelCaps) ResolverOption { return WithExtraCaps(caps) }

// DefaultResolver creates an LLMResolver backed by the DefaultRegistry
// and the given store. The store must implement ProviderConfigStore;
// it may additionally implement ModelConfigStore (for per-model
// overrides) and DefaultModelStore (for Resolve("") routing).
func DefaultResolver(store ProviderConfigStore, opts ...ResolverOption) LLMResolver {
	r := &defaultResolver{
		registry: DefaultRegistry,
		store:    store,
		cache:    make(map[string]LLM),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

type defaultResolver struct {
	registry  *ProviderRegistry
	store     ProviderConfigStore
	fallback  string
	extraCaps ModelCaps

	mu    sync.RWMutex
	cache map[string]LLM
	sf    singleflight.Group
}

// Resolve maps a "provider/model" string to an LLM instance. Empty
// modelStr triggers default-model lookup in this order:
//
//  1. DefaultModelStore.GetDefaultModel (preferred)
//  2. Legacy ProviderConfig row keyed by GlobalDefaultProvider
//     (deprecated, removed in v0.2.0)
//  3. WithFallbackModel
func (r *defaultResolver) Resolve(ctx context.Context, modelStr string) (LLM, error) {
	if modelStr == "" {
		modelStr = r.resolveDefaultModel(ctx)
	}
	if modelStr == "" {
		return nil, errdefs.Validationf("no model specified and no fallback configured")
	}

	r.mu.RLock()
	if cached, ok := r.cache[modelStr]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	r.mu.RUnlock()

	v, err, _ := r.sf.Do(modelStr, func() (any, error) {
		return r.createLLM(ctx, modelStr)
	})
	if err != nil {
		return nil, err
	}
	return v.(LLM), nil
}

// resolveDefaultModel returns the default model identifier or "".
// Tries DefaultModelStore first, then the deprecated GlobalDefaultProvider
// magic key, then WithFallbackModel.
func (r *defaultResolver) resolveDefaultModel(ctx context.Context) string {
	if r.store != nil {
		if dms, ok := r.store.(DefaultModelStore); ok {
			if ref, err := dms.GetDefaultModel(ctx); err == nil && ref != nil &&
				ref.Provider != "" && ref.Model != "" {
				return ref.Provider + "/" + ref.Model
			}
		}
		if gc, err := r.store.GetProviderConfig(ctx, GlobalDefaultProvider); err == nil && gc != nil {
			if p, _ := gc.Config["provider"].(string); p != "" {
				if m, _ := gc.Config["model"].(string); m != "" {
					return p + "/" + m
				}
			}
		}
	}
	return r.fallback
}

func (r *defaultResolver) createLLM(ctx context.Context, modelStr string) (LLM, error) {
	provider, modelName := parseModelString(modelStr)

	pc, err := r.store.GetProviderConfig(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("resolve provider config for %s: %w", provider, err)
	}

	// Per-model overrides are optional. NotFound is silent; any other
	// store error fails resolve so we don't hide misconfiguration.
	var mc *ModelConfig
	if mcs, ok := r.store.(ModelConfigStore); ok {
		got, mErr := mcs.GetModelConfig(ctx, provider, modelName)
		if mErr == nil {
			mc = got
		} else if !errdefs.IsNotFound(mErr) {
			return nil, fmt.Errorf("resolve model config for %s/%s: %w", provider, modelName, mErr)
		}
	}

	// Shallow merge: per-model Extra overwrites provider Config keys.
	// We never deep-merge — predictable, and matches user mental model
	// of "the value I set wins as-is".
	merged := pc.Config
	if mc != nil && len(mc.Extra) > 0 {
		merged = shallowMergeConfig(pc.Config, mc.Extra)
	}

	inst, err := r.registry.NewFromConfig(provider, modelName, merged)
	if err != nil {
		return nil, err
	}

	// Caps merge order (OR — any layer disabling a cap wins):
	//   1) Registry catalog caps for this model
	//   2) ProviderConfig.Caps (user-supplied, applies to all models of provider)
	//   3) Legacy Config["caps"] sub-object (deprecated)
	//   4) ModelConfig.Caps (user-supplied, this model only)
	//   5) Resolver-wide extra caps from WithExtraCaps
	//
	// NewFromConfig already applies (1) + (3) internally and wraps the
	// instance once. We unwrap+rewrap here so the additional layers
	// (2, 4, 5) compose cleanly without nesting CapsMiddleware twice.
	caps := mergeCaps(
		r.registry.LookupModelCaps(provider, modelName),
		pc.Caps,
	)
	if mc != nil {
		caps = mergeCaps(caps, mc.Caps)
	}
	caps = mergeCaps(caps, capsFromConfig(merged), r.extraCaps)
	inst = CapsMiddleware(unwrapCaps(inst), caps)

	r.mu.Lock()
	r.cache[modelStr] = inst
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

func (r *defaultResolver) InvalidateCache(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if provider == "" {
		r.cache = make(map[string]LLM)
		return
	}
	for key := range r.cache {
		p, _ := parseModelString(key)
		if p == provider {
			delete(r.cache, key)
		}
	}
}

// parseModelString splits "provider/model" into (provider, model).
// If there is no "/" separator, the whole string is treated as the model
// name and the provider is inferred as the model name itself (covers
// cases like "gpt-4o" → provider "openai" etc., but we fall back to using
// the raw string as provider which will be resolved by the registry).
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
