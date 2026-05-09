package llm

import (
	"context"
	"testing"
)

type mockLLM struct{}

func (mockLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	return NewTextMessage(RoleAssistant, "mock"), TokenUsage{}, nil
}

func (mockLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return nil, nil
}

func TestProviderRegistry_RegisterAndResolve(t *testing.T) {
	reg := NewProviderRegistry()
	reg.Register("test", func(model string, config map[string]any) (LLM, error) {
		return &mockLLM{}, nil
	})

	providers := reg.ListProviders()
	if len(providers) != 1 || providers[0] != "test" {
		t.Fatalf("ListProviders() = %v, want [test]", providers)
	}

	inst, err := reg.NewFromConfig("test", "model-1", map[string]any{"api_key": "key"})
	if err != nil {
		t.Fatalf("NewFromConfig error: %v", err)
	}
	if inst == nil {
		t.Fatal("NewFromConfig returned nil")
	}
}

func TestProviderRegistry_NotFound(t *testing.T) {
	reg := NewProviderRegistry()
	_, err := reg.NewFromConfig("nonexistent", "", nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestProviderRegistry_Models(t *testing.T) {
	reg := NewProviderRegistry()
	reg.RegisterModels("openai", []ModelInfo{
		{Label: "GPT-4o", Name: "gpt-4o"},
		{Label: "GPT-4o Mini", Name: "gpt-4o-mini"},
	})
	reg.RegisterModels("anthropic", []ModelInfo{
		{Label: "Claude", Name: "claude-3"},
	})

	all := reg.ListAllModels()
	if len(all) != 3 {
		t.Fatalf("ListAllModels() len = %d, want 3", len(all))
	}

	for _, m := range all {
		if m.Provider == "" {
			t.Fatal("Provider should be set on registered models")
		}
	}
}

func TestProviderRegistry_RegisterModels_DoesNotMutateInput(t *testing.T) {
	reg := NewProviderRegistry()
	models := []ModelInfo{
		{Label: "GPT-4o", Name: "gpt-4o"},
		{Label: "GPT-4o Mini", Name: "gpt-4o-mini"},
	}

	reg.RegisterModels("openai", models)

	for _, m := range models {
		if m.Provider != "" {
			t.Fatalf("RegisterModels mutated input slice: Provider = %q, want empty", m.Provider)
		}
	}

	all := reg.ListAllModels()
	for _, m := range all {
		if m.Provider != "openai" {
			t.Fatalf("registered model Provider = %q, want 'openai'", m.Provider)
		}
	}
}

func TestNewFromConfig_ReturnsRawInstance_NoSpecWrap(t *testing.T) {
	// Post-redesign contract: NewFromConfig is the bare-provider entry
	// point; spec wrapping (caps / defaults / limits) is the resolver's
	// job. Even when the catalog declares caps, NewFromConfig must not
	// auto-wrap — otherwise the resolver's one-shot assembly would
	// double-wrap.
	reg := NewProviderRegistry()
	raw := &mockLLM{}
	reg.Register("test", func(model string, config map[string]any) (LLM, error) {
		return raw, nil
	})
	reg.RegisterModels("test", []ModelInfo{
		{Label: "Capped", Name: "capped", Spec: ModelSpec{Caps: DisabledCaps(CapTemperature)}},
	})

	inst, err := reg.NewFromConfig("test", "capped", map[string]any{"api_key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if inst != raw {
		t.Fatal("NewFromConfig must return the raw provider instance unwrapped")
	}
}
