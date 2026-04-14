package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"golang.org/x/sync/singleflight"
)

// ProviderConfig holds provider configuration for LLM resolution.
type ProviderConfig struct {
	Provider string         `json:"provider"`
	Config   map[string]any `json:"config"`
}

// ProviderConfigStore provides provider config lookup for LLM resolution.
type ProviderConfigStore interface {
	GetProviderConfig(ctx context.Context, provider string) (*ProviderConfig, error)
}

// LLMResolver resolves a model string (e.g. "openai/gpt-4o") into an LLM
// instance.
type LLMResolver interface {
	Resolve(ctx context.Context, model string) (LLM, error)
	InvalidateCache(provider string)
}

// GlobalDefaultProvider is the well-known key used to look up the
// global default provider/model from a ProviderConfigStore.
const GlobalDefaultProvider = "__global_default__"

// ResolverOption configures a defaultResolver.
type ResolverOption func(*defaultResolver)

// WithFallbackModel sets the default model used when Resolve("") is called.
func WithFallbackModel(model string) ResolverOption {
	return func(r *defaultResolver) { r.fallback = model }
}

// WithModelCaps merges extra ModelCaps into the resolver. These caps are
// combined with registry caps when wrapping LLM instances.
func WithModelCaps(caps ModelCaps) ResolverOption {
	return func(r *defaultResolver) { r.extraCaps = mergeCaps(r.extraCaps, caps) }
}

// DefaultResolver creates an LLMResolver backed by the DefaultRegistry and
// the given ProviderConfigStore (for provider config / credentials lookup).
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

func (r *defaultResolver) Resolve(ctx context.Context, modelStr string) (LLM, error) {
	if modelStr == "" && r.store != nil {
		if gc, err := r.store.GetProviderConfig(ctx, GlobalDefaultProvider); err == nil {
			if p, _ := gc.Config["provider"].(string); p != "" {
				if m, _ := gc.Config["model"].(string); m != "" {
					modelStr = p + "/" + m
				}
			}
		}
	}
	if modelStr == "" {
		modelStr = r.fallback
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

func (r *defaultResolver) createLLM(ctx context.Context, modelStr string) (LLM, error) {
	provider, modelName := parseModelString(modelStr)

	cfg, err := r.store.GetProviderConfig(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("resolve provider config for %s: %w", provider, err)
	}

	inst, err := r.registry.NewFromConfig(provider, modelName, cfg.Config)
	if err != nil {
		return nil, err
	}
	inst = CapsMiddleware(inst, r.extraCaps)

	r.mu.Lock()
	r.cache[modelStr] = inst
	r.mu.Unlock()

	return inst, nil
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
