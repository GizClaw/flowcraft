package azure

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProvider(t *testing.T) {
	t.Setenv("AZURE_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "azure",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{"name": "gpt-5", "kind": "generate"}]
			},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "${env:AZURE_TEST_KEY}"}
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
	if provider.ID != "azure" || len(provider.Models) != 1 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestResourceFactoryRejectsASRUntilCoreTranscription(t *testing.T) {
	_, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "azure",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{"name": "whisper", "kind": "asr"}]
			},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "sk-test"}
			}]
		}`),
	})
	if err == nil {
		t.Fatal("New accepted an asr deployment before core transcription exists")
	}
}

func TestRegisterAddsAzureProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "azure"); !ok {
		t.Fatalf("factory %s/azure missing", ResourceKind)
	}
}
