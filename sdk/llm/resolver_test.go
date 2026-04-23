package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ---------------------------------------------------------------------------
// Test mocks
// ---------------------------------------------------------------------------

type resolverMockStore struct {
	mu      sync.RWMutex
	configs map[string]*ProviderConfig
}

func newResolverMockStore() *resolverMockStore {
	return &resolverMockStore{configs: make(map[string]*ProviderConfig)}
}

func (s *resolverMockStore) GetProviderConfig(_ context.Context, key string) (*ProviderConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
// Resolver
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

	r.InvalidateCache("p1")
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
	r.InvalidateCache("")
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
