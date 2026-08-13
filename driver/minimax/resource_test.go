package minimax

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProvider(t *testing.T) {
	t.Setenv("MINIMAX_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "minimax",
			"spec": {},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "${env:MINIMAX_TEST_KEY}"}
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
	if provider.ID != "minimax" || len(provider.Models) == 0 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestRegisterAddsMiniMaxProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "minimax"); !ok {
		t.Fatalf("factory %s/minimax missing", ResourceKind)
	}
}
