package llm

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ---------------------------------------------------------------------------
// Test mocks for layered store interfaces
// ---------------------------------------------------------------------------

// layeredMockStore implements ProviderConfigStore + ModelConfigStore +
// DefaultModelStore. Each interface contributes independently so tests
// can isolate one layer at a time by leaving the others nil.
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

func (s *layeredMockStore) GetProviderConfig(_ context.Context, provider string) (*ProviderConfig, error) {
	if pc, ok := s.providers[provider]; ok {
		return pc, nil
	}
	return nil, errdefs.NotFoundf("provider %q not found", provider)
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

func (s *providerOnlyStore) GetProviderConfig(_ context.Context, provider string) (*ProviderConfig, error) {
	if pc, ok := s.providers[provider]; ok {
		return pc, nil
	}
	return nil, errdefs.NotFoundf("provider %q not found", provider)
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

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}
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

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}
	if _, err := r.Resolve(context.Background(), "p/whatever"); err != nil {
		t.Fatalf("NotFound should be silent, got %v", err)
	}
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

func TestResolver_ModelConfig_OtherError_FailsResolve(t *testing.T) {
	base := newLayeredMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	base.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}

	store := &failingModelStore{layeredMockStore: base, err: errdefs.Internalf("db down")}
	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}

	_, err := r.Resolve(context.Background(), "p/m")
	if err == nil {
		t.Fatal("expected non-NotFound model store error to fail Resolve")
	}
}

// ---------------------------------------------------------------------------
// DefaultModelStore — preferred default model lookup
// ---------------------------------------------------------------------------

func TestResolver_DefaultModelStore_PreferredOverGlobalDefault(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	store.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}
	// New API winner.
	store.defaultM = &DefaultModelRef{Provider: "p", Model: "from-default-store"}
	// Legacy fallback also present — must be ignored when DefaultModelStore wins.
	store.providers[GlobalDefaultProvider] = &ProviderConfig{
		Provider: GlobalDefaultProvider,
		Config:   map[string]any{"provider": "p", "model": "from-legacy"},
	}

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}
	inst, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := inst.(*resolverMockLLM).model; got != "from-default-store" {
		t.Fatalf("DefaultModelStore should win, got %q", got)
	}
}

func TestResolver_DefaultModelStore_NotFound_FallsBackToLegacy(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	store.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}
	store.defaultM = nil // GetDefaultModel returns NotFound
	store.providers[GlobalDefaultProvider] = &ProviderConfig{
		Provider: GlobalDefaultProvider,
		Config:   map[string]any{"provider": "p", "model": "from-legacy"},
	}

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}
	inst, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := inst.(*resolverMockLLM).model; got != "from-legacy" {
		t.Fatalf("legacy fallback should kick in, got %q", got)
	}
}

func TestResolver_DefaultModelStore_FallsBackToWithFallback(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &resolverMockLLM{model: model}, nil
	})
	store.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}
	// Both new and legacy default are absent.

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM),
		fallback: "p/hard-fallback"}
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
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &probeProviderLLM{model: model, onGen: func(opts []GenerateOption) {
			capturedOpts = GenerateOptions{}
			for _, o := range opts {
				o(&capturedOpts)
			}
		}}, nil
	})
	// Layer 1: registry catalog disables temperature for "reason-model".
	reg.RegisterModels("p", []ModelInfo{
		{Name: "reason-model", Caps: DisabledCaps(CapTemperature)},
	})
	// Layer 2: ProviderConfig.Caps disables JSON mode for everything under p.
	store.providers["p"] = &ProviderConfig{
		Provider: "p", Config: map[string]any{"api_key": "k"},
		Caps: DisabledCaps(CapJSONMode),
	}
	// Layer 3: ModelConfig.Caps disables JSON schema for this model.
	store.models["p/reason-model"] = &ModelConfig{
		Provider: "p", Model: "reason-model",
		Caps: DisabledCaps(CapJSONSchema),
	}

	// Layer 4: resolver-wide extra caps — none set here, leave for option test.
	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}
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
	// JSONSchema disabled at model layer downgrades to JSONMode (per
	// caps.go semantics); since JSONMode is also disabled the final
	// state is "no schema, no mode".
	if capturedOpts.JSONSchema != nil {
		t.Errorf("json_schema should be downgraded then cleared, got %v", capturedOpts.JSONSchema)
	}
}

func TestResolver_Caps_ExtraFromOption(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()

	var capturedOpts GenerateOptions
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &probeProviderLLM{model: model, onGen: func(opts []GenerateOption) {
			capturedOpts = GenerateOptions{}
			for _, o := range opts {
				o(&capturedOpts)
			}
		}}, nil
	})
	store.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}
	WithExtraCaps(DisabledCaps(CapTemperature))(r)

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

func TestResolver_Caps_LegacyConfigKey_StillRespected(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()

	var capturedOpts GenerateOptions
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &probeProviderLLM{model: model, onGen: func(opts []GenerateOption) {
			capturedOpts = GenerateOptions{}
			for _, o := range opts {
				o(&capturedOpts)
			}
		}}, nil
	})
	// Legacy: caps stuffed into Config["caps"] (the deprecated path).
	store.providers["p"] = &ProviderConfig{
		Provider: "p",
		Config: map[string]any{
			"api_key": "k",
			"caps":    map[string]any{"no_temperature": true},
		},
	}

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}
	llm, err := r.Resolve(context.Background(), "p/m")
	if err != nil {
		t.Fatal(err)
	}
	temp := 0.5
	_, _, _ = llm.Generate(context.Background(), nil, WithTemperature(temp))
	if capturedOpts.Temperature != nil {
		t.Fatalf("legacy Config[\"caps\"] must still strip temperature, got %v", *capturedOpts.Temperature)
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

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM),
		fallback: "p/the-model"}
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
	// base must be untouched.
	if base["b"] != 2 {
		t.Errorf("base mutated: b=%v", base["b"])
	}
}

func TestShallowMergeConfig_EmptyOverlay_ReturnsBaseAsIs(t *testing.T) {
	base := map[string]any{"a": 1}
	got := shallowMergeConfig(base, nil)
	// Documented optimization: returns base as-is to avoid allocation.
	if &got == &base {
		// Pointer comparison on map headers isn't meaningful; instead
		// just check no copy was made by mutating base and seeing got.
	}
	base["a"] = 2
	if got["a"] != 2 {
		t.Errorf("expected zero-overlay path to return base verbatim, got snapshot %v", got["a"])
	}
}

// ---------------------------------------------------------------------------
// Deprecated WithModelCaps still works
// ---------------------------------------------------------------------------

func TestResolver_WithModelCaps_Deprecated_StillForwards(t *testing.T) {
	store := newLayeredMockStore()
	reg := NewProviderRegistry()
	var captured GenerateOptions
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &probeProviderLLM{model: model, onGen: func(opts []GenerateOption) {
			captured = GenerateOptions{}
			for _, o := range opts {
				o(&captured)
			}
		}}, nil
	})
	store.providers["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{"api_key": "k"}}

	r := &defaultResolver{registry: reg, store: store, cache: make(map[string]LLM)}
	WithModelCaps(DisabledCaps(CapTemperature))(r) //nolint:staticcheck // deprecation-compat test
	llm, err := r.Resolve(context.Background(), "p/m")
	if err != nil {
		t.Fatal(err)
	}
	temp := 0.5
	_, _, _ = llm.Generate(context.Background(), nil, WithTemperature(temp))
	if captured.Temperature != nil {
		t.Fatalf("WithModelCaps must still gate caps (deprecated alias), got %v", *captured.Temperature)
	}
}
