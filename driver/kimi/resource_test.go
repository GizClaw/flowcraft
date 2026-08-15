package kimi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProvider(t *testing.T) {
	t.Setenv("KIMI_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "kimi",
			"spec": {},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "${env:KIMI_TEST_KEY}"}
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
	if provider.ID != "kimi" || len(provider.Models) == 0 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestRegisterAddsKimiProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "kimi"); !ok {
		t.Fatalf("factory %s/kimi missing", ResourceKind)
	}
}

func TestProviderCarriesGenerateOptionsDecoder(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "kimi",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	decoder, ok := provider.ExtensionDecoders[extensionGenerate]
	if !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, extensionGenerate)
	}

	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "kimi",
		ID:       extensionGenerate,
		Fields:   json.RawMessage(`{"prompt_cache_key":"sess-1","preserve_thinking":true}`),
	}}, map[string]inference.ExtensionDecoder{
		"kimi/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options := extensions[0].(*GenerateOptions)
	if options.ProviderID() != "kimi" || options.PromptCacheKey != "sess-1" ||
		options.PreserveThinking == nil || !*options.PreserveThinking {
		t.Fatalf("decoded options = %#v", options)
	}

	value, err = Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "km-prod",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decoder = value.(inference.ProviderDefinition).ExtensionDecoders[extensionGenerate]
	extensions, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "km-prod",
		ID:       extensionGenerate,
	}}, map[string]inference.ExtensionDecoder{
		"km-prod/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*GenerateOptions); options.ProviderID() != "km-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "km-prod")
	}
}
