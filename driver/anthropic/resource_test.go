package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "anthropic",
			"spec": {},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "${env:ANTHROPIC_TEST_KEY}"}
			}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider, ok := value.(inference.ProviderDefinition)
	if !ok {
		t.Fatalf("New returned %T, want inference.ProviderDefinition", value)
	}
	if provider.ID != "anthropic" || len(provider.Models) == 0 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestRegisterAddsAnthropicProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "anthropic"); !ok {
		t.Fatalf("factory %s/anthropic missing", ResourceKind)
	}
}
