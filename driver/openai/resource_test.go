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

func TestResourceSettingsHTTPRetriesFromEnv(t *testing.T) {
	t.Setenv("OPENAI_TEST_HTTP_RETRIES", "3")
	settings, err := resource.DecodeTyped[ResourceSettings](
		json.RawMessage(`{
			"id": "openai",
			"spec": {"http_retries": "${env:OPENAI_TEST_HTTP_RETRIES}"}
		}`),
		resource.ExpandEnv())
	if err != nil {
		t.Fatalf("DecodeTyped: %v", err)
	}
	spec, err := decodeSpec(settings.Spec)
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if spec.HTTPRetries == nil || int(*spec.HTTPRetries) != 3 {
		t.Fatalf("HTTPRetries = %v, want 3", spec.HTTPRetries)
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

func TestProviderCarriesGenerateOptionsDecoder(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "openai",
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
		Provider: "openai",
		ID:       extensionGenerate,
		Fields: json.RawMessage(`{
			"web_search": {
				"tool_choice": {"required": true},
				"search_context_size": "high"
			}
		}`),
	}}, map[string]inference.ExtensionDecoder{
		"openai/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options := extensions[0].(*GenerateOptions)
	if options.ProviderID() != "openai" || options.WebSearch == nil ||
		options.WebSearch.ToolChoice == nil || !options.WebSearch.ToolChoice.Required ||
		options.WebSearch.SearchContextSize != "high" {
		t.Fatalf("decoded options = %#v", options)
	}

	// A renamed deployment still decodes: the decoder is bound to the
	// deployment provider ID, not the driver default.
	value, err = Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "oa-prod",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decoder = value.(inference.ProviderDefinition).ExtensionDecoders[extensionGenerate]
	extensions, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "oa-prod",
		ID:       extensionGenerate,
	}}, map[string]inference.ExtensionDecoder{
		"oa-prod/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*GenerateOptions); options.ProviderID() != "oa-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "oa-prod")
	}
}
