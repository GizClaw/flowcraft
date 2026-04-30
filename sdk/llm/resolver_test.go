package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ---------------------------------------------------------------------------
// Test-only resolver constructor
// ---------------------------------------------------------------------------

// newResolverWithRegistry is the test-only constructor that lets us
// inject a custom ProviderRegistry while keeping the rest of the
// resolver's setup identical to the public DefaultResolver — i.e. all
// store-interface dispatch, caching, and option wiring are exercised
// exactly as production code would. Tests should NOT construct
// &defaultResolver{...} directly; do everything through this helper
// plus DefaultResolver-style options.
func newResolverWithRegistry(store ProviderConfigStore, reg *ProviderRegistry, opts ...ResolverOption) LLMResolver {
	r := DefaultResolver(store, opts...).(*defaultResolver)
	r.registry = reg
	return r
}

// ---------------------------------------------------------------------------
// Mocks: provider-only store + minimal LLM stub
// ---------------------------------------------------------------------------

type resolverMockStore struct {
	mu      sync.RWMutex
	configs map[string]*ProviderConfig
}

func newResolverMockStore() *resolverMockStore {
	return &resolverMockStore{configs: make(map[string]*ProviderConfig)}
}

func (s *resolverMockStore) GetProviderConfig(_ context.Context, provider, profile string) (*ProviderConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Tests that only set "<provider>" entries see profile=="" requests as
	// equivalent. Tests that exercise multi-profile behavior set keys of
	// the form "<provider>#<profile>" directly.
	key := provider
	if profile != "" {
		key = provider + "#" + profile
	}
	pc, ok := s.configs[key]
	if !ok {
		return nil, errdefs.NotFoundf("provider_config %q not found", key)
	}
	return pc, nil
}

type resolverMockLLM struct{ model string }

func (m *resolverMockLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	return NewTextMessage(RoleAssistant, "mock:"+m.model), TokenUsage{}, nil
}

func (m *resolverMockLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Mocks: layered store covering ProviderConfigStore + ModelConfigStore +
// DefaultModelStore. Each interface contributes independently so tests
// can isolate one layer at a time by leaving the others nil.
// ---------------------------------------------------------------------------

type layeredMockStore struct {
	providers map[string]*ProviderConfig
	models    map[string]*ModelConfig // key = provider+"/"+model
	defaultM  *DefaultModelRef
}

func newLayeredMockStore() *layeredMockStore {
	return &layeredMockStore{
		providers: make(map[string]*ProviderConfig),
		models:    make(map[string]*ModelConfig),
	}
}

func (s *layeredMockStore) GetProviderConfig(_ context.Context, provider, profile string) (*ProviderConfig, error) {
	key := provider
	if profile != "" {
		key = provider + "#" + profile
	}
	if pc, ok := s.providers[key]; ok {
		return pc, nil
	}
	return nil, errdefs.NotFoundf("provider %q not found", key)
}

func (s *layeredMockStore) GetModelConfig(_ context.Context, provider, model string) (*ModelConfig, error) {
	if mc, ok := s.models[provider+"/"+model]; ok {
		return mc, nil
	}
	return nil, errdefs.NotFoundf("model %q not found", provider+"/"+model)
}

func (s *layeredMockStore) GetDefaultModel(_ context.Context) (*DefaultModelRef, error) {
	if s.defaultM == nil {
		return nil, errdefs.NotFoundf("no default")
	}
	return s.defaultM, nil
}

// providerOnlyStore implements just ProviderConfigStore — used to
// simulate animus-style callers and verify the new optional interfaces
// stay opt-in.
type providerOnlyStore struct{ providers map[string]*ProviderConfig }

func (s *providerOnlyStore) GetProviderConfig(_ context.Context, provider, profile string) (*ProviderConfig, error) {
	if profile != "" {
		return nil, errdefs.NotFoundf("provider %q has no profile %q", provider, profile)
	}
	if pc, ok := s.providers[provider]; ok {
		return pc, nil
	}
	return nil, errdefs.NotFoundf("provider %q not found", provider)
}

// failingModelStore lets us inject a non-NotFound error from
// GetModelConfig and verify it propagates.
type failingModelStore struct {
	*layeredMockStore
	err error
}

func (s *failingModelStore) GetModelConfig(_ context.Context, _, _ string) (*ModelConfig, error) {
	return nil, s.err
}

// probeProviderLLM records which config it was constructed with, so
// shallow-merge tests can verify what hit NewFromConfig.
type probeProviderLLM struct {
	model     string
	gotConfig map[string]any
	onGen     func(opts []GenerateOption)
}

func (p *probeProviderLLM) Generate(_ context.Context, _ []Message, opts ...GenerateOption) (Message, TokenUsage, error) {
	if p.onGen != nil {
		p.onGen(opts)
	}
	return NewTextMessage(RoleAssistant, "ok"), TokenUsage{}, nil
}

func (p *probeProviderLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return nil, nil
}

// captureGenOpts returns a factory that records the GenerateOption set
// applied by the most recent Generate call into *into. Used by caps
// tests to assert which user-supplied options survived CapsMiddleware.
func captureGenOpts(reg *ProviderRegistry, provider string, into *GenerateOptions) {
	reg.Register(provider, func(model string, _ map[string]any) (LLM, error) {
		return &probeProviderLLM{model: model, onGen: func(opts []GenerateOption) {
			*into = GenerateOptions{}
			for _, o := range opts {
				o(into)
			}
		}}, nil
	})
}

// ---------------------------------------------------------------------------
// parseModelString / inferProvider
// ---------------------------------------------------------------------------

func TestParseModelString_WithSlash(t *testing.T) {
	provider, model := parseModelString("openai/gpt-4o")
	if provider != "openai" || model != "gpt-4o" {
		t.Fatalf("got (%q, %q), want (openai, gpt-4o)", provider, model)
	}
}

func TestParseModelString_WithoutSlash(t *testing.T) {
	provider, model := parseModelString("gpt-4o")
	if provider != "openai" || model != "gpt-4o" {
		t.Fatalf("got (%q, %q), want (openai, gpt-4o)", provider, model)
	}
}

func TestInferProvider(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"gpt-4o", "openai"},
		{"o1-mini", "openai"},
		{"o3", "openai"},
		{"claude-3-5-sonnet", "anthropic"},
		{"deepseek-chat", "deepseek"},
		{"qwen-max", "qwen"},
		{"doubao-pro", "bytedance"},
		{"abab6.5", "minimax"},
		{"llama3", "ollama"},
		{"mistral-7b", "ollama"},
		{"gemma-2b", "ollama"},
		{"unknown-model", "unknown-model"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := inferProvider(tt.model)
			if got != tt.want {
				t.Fatalf("inferProvider(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestRegisterInferRule(t *testing.T) {
	RegisterInferRule("gemini-", "google")
	got := inferProvider("gemini-pro")
	if got != "google" {
		t.Fatalf("inferProvider(%q) = %q, want %q", "gemini-pro", got, "google")
	}
}

// ---------------------------------------------------------------------------
// Resolver — caching, invalidation, fallback, concurrency
// ---------------------------------------------------------------------------

func TestResolver_CacheHit(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()

	var factoryCalls atomic.Int32
	reg.Register("test-provider", func(model string, config map[string]any) (LLM, error) {
		factoryCalls.Add(1)
		return &resolverMockLLM{model: model}, nil
	})
	store.configs["test-provider"] = &ProviderConfig{
		Provider: "test-provider",
		Config:   map[string]any{"api_key": "key"},
	}

	r := newResolverWithRegistry(store, reg)

	ctx := context.Background()
	_, _ = r.Resolve(ctx, "test-provider/model-a")
	_, _ = r.Resolve(ctx, "test-provider/model-a")
	_, _ = r.Resolve(ctx, "test-provider/model-a")

	if factoryCalls.Load() != 1 {
		t.Fatalf("expected factory called once (cached), called %d times", factoryCalls.Load())
	}
}

func TestResolver_InvalidateCache_ByProvider(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()

	var factoryCalls atomic.Int32
	reg.Register("p1", func(model string, config map[string]any) (LLM, error) {
		factoryCalls.Add(1)
		return &resolverMockLLM{model: model}, nil
	})
	reg.Register("p2", func(model string, config map[string]any) (LLM, error) {
		factoryCalls.Add(1)
		return &resolverMockLLM{model: model}, nil
	})
	store.configs["p1"] = &ProviderConfig{Provider: "p1", Config: map[string]any{"api_key": "k1"}}
	store.configs["p2"] = &ProviderConfig{Provider: "p2", Config: map[string]any{"api_key": "k2"}}

	r := newResolverWithRegistry(store, reg)
	ctx := context.Background()

	_, _ = r.Resolve(ctx, "p1/model-a")
	_, _ = r.Resolve(ctx, "p2/model-b")
	if factoryCalls.Load() != 2 {
		t.Fatalf("expected 2 initial calls, got %d", factoryCalls.Load())
	}

	r.InvalidateCache(WithProvider("p1"))
	_, _ = r.Resolve(ctx, "p1/model-a")
	if factoryCalls.Load() != 3 {
		t.Fatalf("expected p1 cache miss after invalidate, got %d calls", factoryCalls.Load())
	}

	_, _ = r.Resolve(ctx, "p2/model-b")
	if factoryCalls.Load() != 3 {
		t.Fatal("expected p2 still cached")
	}
}

func TestResolver_InvalidateCache_All(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()

	var factoryCalls atomic.Int32
	reg.Register("prov", func(model string, config map[string]any) (LLM, error) {
		factoryCalls.Add(1)
		return &resolverMockLLM{model: model}, nil
	})
	store.configs["prov"] = &ProviderConfig{Provider: "prov", Config: map[string]any{"api_key": "k"}}

	r := newResolverWithRegistry(store, reg)
	ctx := context.Background()

	_, _ = r.Resolve(ctx, "prov/m1")
	_, _ = r.Resolve(ctx, "prov/m2")
	r.InvalidateCache()
	_, _ = r.Resolve(ctx, "prov/m1")
	_, _ = r.Resolve(ctx, "prov/m2")

	if factoryCalls.Load() != 4 {
		t.Fatalf("expected 4 calls after full invalidate, got %d", factoryCalls.Load())
	}
}

func TestResolver_FallbackModel(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	reg.Register("fb-prov", func(model string, config map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	store.configs["fb-prov"] = &ProviderConfig{Provider: "fb-prov", Config: map[string]any{"api_key": "k"}}

	r := newResolverWithRegistry(store, reg, WithFallbackModel("fb-prov/default-model"))

	inst, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if inst.(*resolverMockLLM).model != "default-model" {
		t.Fatalf("expected fallback model, got %q", inst.(*resolverMockLLM).model)
	}
}

func TestResolver_ExplicitModel_OverridesDefault(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	reg.Register("prov", func(model string, config map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	store.configs["prov"] = &ProviderConfig{Provider: "prov", Config: map[string]any{"api_key": "k"}}

	r := newResolverWithRegistry(store, reg, WithFallbackModel("prov/should-not-win"))

	inst, err := r.Resolve(context.Background(), "prov/explicit-model")
	if err != nil {
		t.Fatal(err)
	}
	if inst.(*resolverMockLLM).model != "explicit-model" {
		t.Fatalf("expected explicit model override, got %q", inst.(*resolverMockLLM).model)
	}
}

func TestResolver_ConcurrentResolve(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	reg.Register("prov", func(model string, config map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	store.configs["prov"] = &ProviderConfig{Provider: "prov", Config: map[string]any{"api_key": "k"}}

	r := newResolverWithRegistry(store, reg)

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Resolve(context.Background(), "prov/concurrent-model"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent resolve error: %v", err)
	}
}

func TestResolver_NoModelNoFallback(t *testing.T) {
	store := newResolverMockStore()
	r := newResolverWithRegistry(store, NewProviderRegistry())

	_, err := r.Resolve(context.Background(), "")
	if err == nil {
		t.Fatal("expected error when no model and no fallback")
	}
}

func TestResolver_CapsMiddleware_Integration(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	reg.Register("test-prov", func(model string, config map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	reg.RegisterModels("test-prov", []ModelInfo{
		{Label: "Test Model", Name: "capped-model", Caps: DisabledCaps(CapTemperature)},
	})
	store.configs["test-prov"] = &ProviderConfig{Provider: "test-prov", Config: map[string]any{"api_key": "k"}}

	r := newResolverWithRegistry(store, reg)

	inst, err := r.Resolve(context.Background(), "test-prov/capped-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inst.(*capsLLM); !ok {
		t.Fatalf("expected capsLLM wrapper, got %T", inst)
	}
}

// ---------------------------------------------------------------------------
// ModelConfigStore — per-model overrides
// ---------------------------------------------------------------------------

func TestResolver_ModelConfig_OverridesProviderConfig(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()

	var captured *probeProviderLLM
	reg.Register("openai", func(model string, cfg map[string]any) (LLM, error) {
		captured = &probeProviderLLM{model: model, gotConfig: cfg}
		return captured, nil
	})

	store.providers["openai"] = &ProviderConfig{
		Provider: "openai",
		Config:   map[string]any{"api_key": "shared", "base_url": "https://api.openai.com"},
	}
	store.models["openai/azure-gpt"] = &ModelConfig{
		Provider: "openai", Model: "azure-gpt",
		Extra: map[string]any{"base_url": "https://azure-proxy.example.com"},
	}

	r := newResolverWithRegistry(store, reg)
	if _, err := r.Resolve(context.Background(), "openai/azure-gpt"); err != nil {
		t.Fatal(err)
	}

	if captured.gotConfig["api_key"] != "shared" {
		t.Errorf("api_key: want %q, got %q", "shared", captured.gotConfig["api_key"])
	}
	if got := captured.gotConfig["base_url"]; got != "https://azure-proxy.example.com" {
		t.Errorf("base_url override: want azure-proxy, got %q", got)
	}
}

func TestResolver_ModelConfig_NotFound_IsSilent(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	store.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}
	// no models entry — GetModelConfig returns NotFound

	r := newResolverWithRegistry(store, reg)
	if _, err := r.Resolve(context.Background(), "p/whatever"); err != nil {
		t.Fatalf("NotFound should be silent, got %v", err)
	}
}

func TestResolver_ModelConfig_OtherError_FailsResolve(t *testing.T) {
	base := newLayeredMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	base.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}

	store := &failingModelStore{layeredMockStore: base, err: errdefs.Internalf("db down")}
	r := newResolverWithRegistry(store, reg)

	_, err := r.Resolve(context.Background(), "p/m")
	if err == nil {
		t.Fatal("expected non-NotFound model store error to fail Resolve")
	}
}

// ---------------------------------------------------------------------------
// DefaultModelStore — preferred default model lookup
// ---------------------------------------------------------------------------

func TestResolver_DefaultModelStore_FallsBackToWithFallback(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	store.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}
	// No default set anywhere — WithFallbackModel must take over.

	r := newResolverWithRegistry(store, reg, WithFallbackModel("p/hard-fallback"))
	inst, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := inst.(*resolverMockLLM).model; got != "hard-fallback" {
		t.Fatalf("fallback should kick in, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Caps layered merge — registry × provider × model × extra
// ---------------------------------------------------------------------------

func TestResolver_Caps_LayeredMerge(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()

	var capturedOpts GenerateOptions
	captureGenOpts(reg, "p", &capturedOpts)

	// Layer 1: registry catalog disables temperature for "reason-model".
	reg.RegisterModels("p", []ModelInfo{
		{Name: "reason-model", Caps: DisabledCaps(CapTemperature)},
	})
	// Layer 2: ProviderConfig.SpecOverride disables JSON mode for everything under p.
	store.providers["p"] = &ProviderConfig{
		Provider: "p", Config: map[string]any{"api_key": "k"},
		SpecOverride: ModelSpec{Caps: DisabledCaps(CapJSONMode)},
	}
	// Layer 3: ModelConfig.SpecOverride disables JSON schema for this model.
	store.models["p/reason-model"] = &ModelConfig{
		Provider: "p", Model: "reason-model",
		SpecOverride: ModelSpec{Caps: DisabledCaps(CapJSONSchema)},
	}

	r := newResolverWithRegistry(store, reg)
	llm, err := r.Resolve(context.Background(), "p/reason-model")
	if err != nil {
		t.Fatal(err)
	}

	temp, jm := 0.5, true
	schema := JSONSchemaParam{Name: "x", Schema: map[string]any{"type": "object"}}
	_, _, _ = llm.Generate(context.Background(), nil,
		WithTemperature(temp), WithJSONMode(jm), WithJSONSchema(schema))

	if capturedOpts.Temperature != nil {
		t.Errorf("temperature should be stripped (registry layer), got %v", *capturedOpts.Temperature)
	}
	if capturedOpts.JSONMode != nil {
		t.Errorf("json_mode should be stripped (provider layer), got %v", *capturedOpts.JSONMode)
	}
	// JSONSchema is disabled at the model layer; with JSONMode also
	// disabled at the provider layer, no schema fallback survives.
	if capturedOpts.JSONSchema != nil {
		t.Errorf("json_schema should be downgraded then cleared, got %v", capturedOpts.JSONSchema)
	}
}

func TestResolver_Caps_ExtraFromOption(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()

	var capturedOpts GenerateOptions
	captureGenOpts(reg, "p", &capturedOpts)
	store.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}

	r := newResolverWithRegistry(store, reg, WithExtraCaps(DisabledCaps(CapTemperature)))
	llm, err := r.Resolve(context.Background(), "p/m")
	if err != nil {
		t.Fatal(err)
	}
	temp := 0.7
	_, _, _ = llm.Generate(context.Background(), nil, WithTemperature(temp))
	if capturedOpts.Temperature != nil {
		t.Fatalf("WithExtraCaps should disable temperature, got %v", *capturedOpts.Temperature)
	}
}

// ---------------------------------------------------------------------------
// Backward compatibility — animus-style provider-only stores
// ---------------------------------------------------------------------------

func TestResolver_ProviderOnlyStore_NoBehaviorChange(t *testing.T) {
	store := &providerOnlyStore{providers: map[string]*ProviderConfig{
		"p": {Provider: "p", Config: map[string]any{"api_key": "k"}},
	}}
	reg := NewProviderRegistry()
	var calls atomic.Int32
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		calls.Add(1)
		return &resolverMockLLM{model: model}, nil
	})

	r := newResolverWithRegistry(store, reg, WithFallbackModel("p/the-model"))
	if _, err := r.Resolve(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 factory call, got %d", calls.Load())
	}
}

// ---------------------------------------------------------------------------
// shallowMergeConfig
// ---------------------------------------------------------------------------

func TestShallowMergeConfig_OverlayWins(t *testing.T) {
	base := map[string]any{"a": 1, "b": 2}
	overlay := map[string]any{"b": 99, "c": 3}
	got := shallowMergeConfig(base, overlay)
	want := map[string]any{"a": 1, "b": 99, "c": 3}
	if len(got) != len(want) {
		t.Fatalf("size: want %d, got %d", len(want), len(got))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: want %v, got %v", k, v, got[k])
		}
	}
	if base["b"] != 2 {
		t.Errorf("base mutated: b=%v", base["b"])
	}
}

func TestShallowMergeConfig_EmptyOverlay_ReturnsBaseAsIs(t *testing.T) {
	base := map[string]any{"a": 1}
	got := shallowMergeConfig(base, nil)
	// Documented optimization: returns base verbatim. Mutate base and
	// observe got changes — the only black-box way to assert "no copy".
	base["a"] = 2
	if got["a"] != 2 {
		t.Errorf("expected zero-overlay path to return base verbatim, got snapshot %v", got["a"])
	}
}

// ---------------------------------------------------------------------------
// Profile routing — end-to-end ctx-tagged profile picks the right
// ProviderConfig record, missing profile fails loud, default profile
// stays untouched, and per-profile invalidation has the expected
// scope.
//
// Documented contracts under test:
//   - doc/sdk-llm-redesign.md §3.4: WithCredentialProfile installs
//     the profile; CredentialProfileFromContext reads it; the
//     resolver passes it verbatim to ProviderConfigStore.
//   - §3.4 strict-match policy: missing profile errors instead of
//     silently falling back to the default profile.
//   - §5: WithProfile narrows InvalidateCache to one credential
//     without disturbing other profiles or providers.
// ---------------------------------------------------------------------------

func TestResolver_Profile_RoutesToTaggedConfig(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(_ string, config map[string]any) (LLM, error) {
		// Bake the api_key into the model name so we can assert which
		// credential record the factory actually saw.
		return &resolverMockLLM{model: config["api_key"].(string)}, nil
	})
	store.configs["p"] = &ProviderConfig{
		Provider: "p", Config: map[string]any{"api_key": "default-key"},
	}
	store.configs["p#tenant-a"] = &ProviderConfig{
		Provider: "p", Profile: "tenant-a",
		Config: map[string]any{"api_key": "tenant-a-key"},
	}

	r := newResolverWithRegistry(store, reg)

	// Untagged ctx → default profile entry.
	def, err := r.Resolve(context.Background(), "p/m")
	if err != nil {
		t.Fatal(err)
	}
	if got := def.(*resolverMockLLM).model; got != "default-key" {
		t.Errorf("default profile: want 'default-key', got %q", got)
	}

	// Tagged ctx → tenant-a entry.
	ctx := WithCredentialProfile(context.Background(), "tenant-a")
	tA, err := r.Resolve(ctx, "p/m")
	if err != nil {
		t.Fatal(err)
	}
	if got := tA.(*resolverMockLLM).model; got != "tenant-a-key" {
		t.Errorf("tagged profile: want 'tenant-a-key', got %q", got)
	}

	// Default and tenant-a must be cached separately — re-resolving
	// with each ctx returns the same instance respectively.
	def2, _ := r.Resolve(context.Background(), "p/m")
	if def2 != def {
		t.Error("default-profile cache should be hit on re-resolve")
	}
	tA2, _ := r.Resolve(ctx, "p/m")
	if tA2 != tA {
		t.Error("tenant-a-profile cache should be hit on re-resolve")
	}
	if def == tA {
		t.Fatal("default and tenant-a must be distinct cached instances")
	}
}

func TestResolver_Profile_MissingProfile_FailsLoud(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(_ string, _ map[string]any) (LLM, error) {
		return &resolverMockLLM{}, nil
	})
	// Only the default-profile entry exists.
	store.configs["p"] = &ProviderConfig{
		Provider: "p", Config: map[string]any{"api_key": "k"},
	}

	r := newResolverWithRegistry(store, reg)
	ctx := WithCredentialProfile(context.Background(), "ghost")
	_, err := r.Resolve(ctx, "p/m")
	if err == nil {
		t.Fatal("missing profile must fail; silent fallback to default would be a billing/safety incident")
	}
}

func TestResolver_InvalidateCache_ByProfile_ScopesToOneTenant(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	var factoryCalls atomic.Int32
	reg.Register("p", func(_ string, _ map[string]any) (LLM, error) {
		factoryCalls.Add(1)
		return &resolverMockLLM{}, nil
	})
	store.configs["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "default"}}
	store.configs["p#tenant-a"] = &ProviderConfig{Provider: "p", Profile: "tenant-a", Config: map[string]any{"api_key": "a"}}
	store.configs["p#tenant-b"] = &ProviderConfig{Provider: "p", Profile: "tenant-b", Config: map[string]any{"api_key": "b"}}

	r := newResolverWithRegistry(store, reg)
	ctxA := WithCredentialProfile(context.Background(), "tenant-a")
	ctxB := WithCredentialProfile(context.Background(), "tenant-b")

	_, _ = r.Resolve(context.Background(), "p/m")
	_, _ = r.Resolve(ctxA, "p/m")
	_, _ = r.Resolve(ctxB, "p/m")
	if factoryCalls.Load() != 3 {
		t.Fatalf("expected 3 initial factory calls (one per profile), got %d", factoryCalls.Load())
	}

	// Evict ONLY tenant-a.
	r.InvalidateCache(WithProvider("p"), WithProfile("tenant-a"))

	_, _ = r.Resolve(ctxA, "p/m")
	if factoryCalls.Load() != 4 {
		t.Errorf("tenant-a should have re-instantiated, calls=%d", factoryCalls.Load())
	}
	_, _ = r.Resolve(ctxB, "p/m")
	_, _ = r.Resolve(context.Background(), "p/m")
	if factoryCalls.Load() != 4 {
		t.Errorf("tenant-b and default should still be cached, calls=%d", factoryCalls.Load())
	}
}

func TestResolver_InvalidateCache_ByProfile_AcrossProviders(t *testing.T) {
	// WithProfile alone (no WithProvider) clears every provider's
	// matching profile — the AND-filter is independent on each
	// dimension, see doc §5.
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	var factoryCalls atomic.Int32
	for _, p := range []string{"p1", "p2"} {
		reg.Register(p, func(_ string, _ map[string]any) (LLM, error) {
			factoryCalls.Add(1)
			return &resolverMockLLM{}, nil
		})
		store.configs[p+"#shared"] = &ProviderConfig{Provider: p, Profile: "shared", Config: map[string]any{}}
		store.configs[p] = &ProviderConfig{Provider: p, Config: map[string]any{}}
	}

	r := newResolverWithRegistry(store, reg)
	ctxShared := WithCredentialProfile(context.Background(), "shared")
	_, _ = r.Resolve(ctxShared, "p1/m")
	_, _ = r.Resolve(ctxShared, "p2/m")
	_, _ = r.Resolve(context.Background(), "p1/m")
	_, _ = r.Resolve(context.Background(), "p2/m")
	if factoryCalls.Load() != 4 {
		t.Fatalf("expected 4 initial calls, got %d", factoryCalls.Load())
	}

	r.InvalidateCache(WithProfile("shared"))

	_, _ = r.Resolve(ctxShared, "p1/m")
	_, _ = r.Resolve(ctxShared, "p2/m")
	if factoryCalls.Load() != 6 {
		t.Errorf("both providers' shared profile should re-instantiate, calls=%d", factoryCalls.Load())
	}
	_, _ = r.Resolve(context.Background(), "p1/m")
	_, _ = r.Resolve(context.Background(), "p2/m")
	if factoryCalls.Load() != 6 {
		t.Errorf("default-profile entries should remain cached, calls=%d", factoryCalls.Load())
	}
}

// Coverage for the cross-middleware contract documented in
// doc/sdk-llm-redesign.md §3.6 (defaults → caps → limits) and §4
// (one-shot resolver assembly, no unwrap dance).

// ---------------------------------------------------------------------------
// §3.6 — three-layer composition order
// ---------------------------------------------------------------------------

// TestCompose_DefaultsThenCapsThenLimits validates the full chain:
//
//  1. Defaults installs Temperature=0.7 and MaxTokens=999 because the
//     caller left both nil.
//  2. Caps disables Temperature → it gets stripped to nil even though
//     the default just set it (caps run AFTER defaults).
//  3. Limits clamps MaxTokens=999 down to MaxOutputTokens=100 (limits
//     run AFTER caps; if caps had stripped MaxTokens nothing would
//     need clamping — but here MaxTokens stays supported).
//
// Failure of any of the three steps produces a distinct symptom on
// the captured opts so a regression points right at the broken layer.
func TestCompose_DefaultsThenCapsThenLimits(t *testing.T) {
	inner := &capsMockLLM{}
	defaults := GenerateOptions{
		Temperature: floatPtr(0.7),
		MaxTokens:   int64Ptr(999),
	}
	chain := WithDefaults(
		WithCaps(
			WithLimits(inner, ModelLimits{MaxOutputTokens: 100}),
			DisabledCaps(CapTemperature),
		),
		defaults,
	)

	_, _, _ = chain.Generate(context.Background(), nil) // no caller opts at all

	if inner.lastOpts.Temperature != nil {
		t.Errorf("Temperature should be nil — defaults set it but caps must drop it: got %v", *inner.lastOpts.Temperature)
	}
	if inner.lastOpts.MaxTokens == nil {
		t.Fatal("MaxTokens should have been installed by defaults")
	}
	if *inner.lastOpts.MaxTokens != 100 {
		t.Errorf("MaxTokens should be clamped to 100 by limits; got %d", *inner.lastOpts.MaxTokens)
	}
}

// TestCompose_CallerWinsOverDefaults_AndStillObeysCaps verifies the
// "caller > defaults" invariant survives wrapping while caps still
// have final say over what the model sees.
func TestCompose_CallerWinsOverDefaults_AndStillObeysCaps(t *testing.T) {
	inner := &capsMockLLM{}
	chain := WithDefaults(
		WithCaps(inner, DisabledCaps(CapTopP)),
		GenerateOptions{TopP: floatPtr(0.5)}, // default TopP=0.5
	)

	// Caller overrides TopP=0.99. Caps then disables TopP entirely.
	// Final: nil (caps wins over both default and caller).
	_, _, _ = chain.Generate(context.Background(), nil, WithTopP(0.99))
	if inner.lastOpts.TopP != nil {
		t.Fatalf("CapTopP must drop TopP regardless of caller / defaults: got %v", *inner.lastOpts.TopP)
	}
}

// ---------------------------------------------------------------------------
// §4 — resolver one-shot assembly: no double-wrap, exact wrap order
// ---------------------------------------------------------------------------

// TestResolver_AssemblyOrder_DefaultsOuterCapsMiddleLimitsInner asserts
// the chain shape resolver builds when all three spec fields are non-zero.
// The inner-most wrapper must be limits, then caps, then defaults outermost
// so the per-call invocation flows defaults → caps → limits → provider.
func TestResolver_AssemblyOrder_DefaultsOuterCapsMiddleLimitsInner(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	raw := &capsMockLLM{}
	reg.Register("p", func(_ string, _ map[string]any) (LLM, error) { return raw, nil })
	reg.RegisterModels("p", []ModelInfo{
		{Name: "m", Spec: ModelSpec{
			Caps:     DisabledCaps(CapTemperature),
			Defaults: GenerateOptions{TopP: floatPtr(0.5)},
			Limits:   ModelLimits{MaxOutputTokens: 100},
		}},
	})
	store.configs["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{}}

	r := newResolverWithRegistry(store, reg)
	inst, err := r.Resolve(context.Background(), "p/m")
	if err != nil {
		t.Fatal(err)
	}

	// Outermost must be defaults.
	d, ok := inst.(*defaultsLLM)
	if !ok {
		t.Fatalf("outermost wrapper: want *defaultsLLM, got %T", inst)
	}
	// Then caps.
	c, ok := d.inner.(*capsLLM)
	if !ok {
		t.Fatalf("layer 2: want *capsLLM, got %T", d.inner)
	}
	// Then limits.
	l, ok := c.inner.(*limitsLLM)
	if !ok {
		t.Fatalf("layer 3: want *limitsLLM, got %T", c.inner)
	}
	// Innermost is the raw provider (no double-wrap).
	if l.inner != LLM(raw) {
		t.Fatalf("innermost: want raw provider, got %T", l.inner)
	}
}

// TestResolver_AssemblyOrder_SkipsZeroLayers proves the resolver does
// NOT introduce dummy wrappers for zero-value spec layers — the docs
// promise "no-op when input is zero, no struct in the chain".
func TestResolver_AssemblyOrder_SkipsZeroLayers(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	raw := &capsMockLLM{}
	reg.Register("p", func(_ string, _ map[string]any) (LLM, error) { return raw, nil })
	// No spec at all — every wrapper should short-circuit.
	reg.RegisterModels("p", []ModelInfo{{Name: "m"}})
	store.configs["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{}}

	r := newResolverWithRegistry(store, reg)
	inst, err := r.Resolve(context.Background(), "p/m")
	if err != nil {
		t.Fatal(err)
	}
	if inst != LLM(raw) {
		t.Fatalf("zero spec must yield raw provider; got %T", inst)
	}
}

// TestResolver_AssemblyOrder_OnlyCaps proves partial specs only wrap
// the relevant layer. With caps but zero defaults / limits, the chain
// is exactly capsLLM → raw (no defaults / limits wrappers).
func TestResolver_AssemblyOrder_OnlyCaps(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	raw := &capsMockLLM{}
	reg.Register("p", func(_ string, _ map[string]any) (LLM, error) { return raw, nil })
	reg.RegisterModels("p", []ModelInfo{
		{Name: "m", Spec: ModelSpec{Caps: DisabledCaps(CapTemperature)}},
	})
	store.configs["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{}}

	r := newResolverWithRegistry(store, reg)
	inst, err := r.Resolve(context.Background(), "p/m")
	if err != nil {
		t.Fatal(err)
	}
	c, ok := inst.(*capsLLM)
	if !ok {
		t.Fatalf("partial spec (caps-only): outer wrapper want *capsLLM, got %T", inst)
	}
	if c.inner != LLM(raw) {
		t.Fatalf("partial spec (caps-only): inner want raw, got %T", c.inner)
	}
}
