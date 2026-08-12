package openai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProviderWithEnvSecret(t *testing.T) {
	t.Setenv("OPENAI_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "openai",
			"spec": {"organization": "org-1"},
			"profiles": [{
				"id": "default",
				"operations": ["generate", "embed"],
				"secrets": {"api_key": "${env:OPENAI_TEST_KEY}"}
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
	if provider.ID != "openai" || len(provider.Profiles) != 1 ||
		len(provider.Models) != len(catalog) {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestResourceFactoryRejectsMissingIDAndSecret(t *testing.T) {
	if _, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{"profiles":[]}`),
	}); err == nil {
		t.Fatal("New accepted settings without id")
	}
	if _, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "openai",
			"profiles": [{"id": "default"}]
		}`),
	}); err == nil {
		t.Fatal("New accepted a profile without api_key")
	}
}

func TestRegisterAddsOpenAIProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "openai"); !ok {
		t.Fatalf("factory %s/openai missing", ResourceKind)
	}
}
