package deepseek

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProviderWithEnvSecret(t *testing.T) {
	t.Setenv("DEEPSEEK_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "deepseek",
			"spec": {"api": "chat"},
			"profiles": [{
				"id": "default",
				"operations": ["generate"],
				"secrets": {"api_key": "${env:DEEPSEEK_TEST_KEY}"}
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
	if provider.ID != "deepseek" || len(provider.Profiles) != 1 ||
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
			"id": "deepseek",
			"profiles": [{"id": "default"}]
		}`),
	}); err == nil {
		t.Fatal("New accepted a profile without api_key")
	}
}

func TestResponsesProviderRejectsUnsupportedModels(t *testing.T) {
	_, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "deepseek",
			"spec": {
				"api": "responses",
				"models": [{"name": "my-chat-only-model", "kind": "generate"}]
			},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "sk-test"}
			}]
		}`),
	})
	if err == nil {
		t.Fatal("responses provider accepted deepseek-v4-pro")
	}
}

func TestResponsesProviderAllowsDeclaredFlashOverride(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "deepseek",
			"spec": {
			"api": "responses",
				"models": [{
					"name": "deepseek-v4-flash",
					"kind": "generate",
					"responses": true,
					"web_search": true
				}]
			},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "sk-test"}
			}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider, ok := value.(inference.ProviderDefinition)
	if !ok || len(provider.Models) != 1 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestRegisterAddsDeepSeekProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "deepseek"); !ok {
		t.Fatalf("factory %s/deepseek missing", ResourceKind)
	}
}
