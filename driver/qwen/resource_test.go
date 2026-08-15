package qwen

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProvider(t *testing.T) {
	t.Setenv("QWEN_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "qwen",
			"spec": {},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "${env:QWEN_TEST_KEY}"}
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
	if provider.ID != "qwen" || len(provider.Models) == 0 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestRegisterAddsQwenProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "qwen"); !ok {
		t.Fatalf("factory %s/qwen missing", ResourceKind)
	}
}

func TestProviderCarriesExtensionDecoders(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "qwen",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	decoders := map[string]inference.ExtensionDecoder{}
	for id, decoder := range provider.ExtensionDecoders {
		decoders["qwen/"+id] = decoder
	}
	if _, ok := decoders["qwen/"+extensionGenerate]; !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, extensionGenerate)
	}
	if _, ok := decoders["qwen/"+extensionEmbed]; !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, extensionEmbed)
	}

	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{
		{
			Provider: "qwen",
			ID:       extensionGenerate,
			Fields:   json.RawMessage(`{"thinking_budget":100,"top_k":20}`),
		},
		{
			Provider: "qwen",
			ID:       extensionEmbed,
			Fields:   json.RawMessage(`{"text_type":"query","instruct":"rank by relevance"}`),
		},
	}, decoders, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	generate, ok := extensions[0].(*GenerateOptions)
	if !ok || generate.ProviderID() != "qwen" || generate.ThinkingBudget == nil ||
		*generate.ThinkingBudget != 100 || generate.TopK == nil || *generate.TopK != 20 {
		t.Fatalf("decoded generate options = %#v", extensions[0])
	}
	embed, ok := extensions[1].(*EmbedOptions)
	if !ok || embed.ProviderID() != "qwen" || embed.TextType != "query" ||
		embed.Instruct != "rank by relevance" {
		t.Fatalf("decoded embed options = %#v", extensions[1])
	}

	// A renamed deployment still decodes: decoders are bound to the
	// deployment provider ID, not the driver default.
	value, err = Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "qw-prod",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decoder := value.(inference.ProviderDefinition).ExtensionDecoders[extensionGenerate]
	extensions, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "qw-prod",
		ID:       extensionGenerate,
	}}, map[string]inference.ExtensionDecoder{
		"qw-prod/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*GenerateOptions); options.ProviderID() != "qw-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "qw-prod")
	}
}
