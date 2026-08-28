package deepseek

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
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

func TestResourceFactoryRequiresID(t *testing.T) {
	if _, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{"profiles":[]}`),
	}); err == nil {
		t.Fatal("New accepted settings without id")
	}
	// A profile without api_key builds fine: missing keys surface at
	// request time (lazy secret resolution), not at deploy time.
	if _, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "deepseek",
			"profiles": [{"id": "default"}]
		}`),
	}); err != nil {
		t.Fatalf("New rejected a profile without api_key: %v", err)
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

func TestResponsesProviderAllowsDeclaredOverride(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "deepseek",
			"spec": {
			"api": "responses",
				"models": [{
					"name": "deepseek-v4-flash",
					"kind": "generate",
					"responses": true,
					"capabilities": {
						"outputs": ["text"],
						"hosted_web_search": true
					}
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
	if !ok || len(provider.Models) != 2 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestResponsesProviderBuildsBothV4Models(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "deepseek",
			"spec": {"api": "responses"},
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
	if !ok {
		t.Fatalf("New returned %T, want inference.ProviderDefinition", value)
	}
	names := make(map[string]bool)
	for _, model := range provider.Models {
		names[model.Descriptor.ID.Name] = true
	}
	if !names["deepseek-v4-flash"] || !names["deepseek-v4-pro"] {
		t.Fatalf("responses provider models = %v, want flash + pro", names)
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

func TestProviderCarriesGenerateOptionsDecoder(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "deepseek",
			"spec": {"api": "responses"},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "sk-test"}
			}]
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
		Provider: "deepseek",
		ID:       extensionGenerate,
		Fields: json.RawMessage(`{
			"web_search": {
				"tool_choice": {"required": true},
				"search_context_size": "high"
			}
		}`),
	}}, map[string]inference.ExtensionDecoder{
		"deepseek/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options, ok := extensions[0].(*GenerateOptions)
	if !ok {
		t.Fatalf("decoded extension = %T, want *GenerateOptions", extensions[0])
	}
	if options.ProviderID() != "deepseek" || options.ExtensionID() != extensionGenerate ||
		options.WebSearch == nil || options.WebSearch.ToolChoice == nil ||
		!options.WebSearch.ToolChoice.Required ||
		options.WebSearch.SearchContextSize != "high" {
		t.Fatalf("decoded options = %#v", options)
	}
}

func TestProviderDecoderBindsDeploymentID(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "ds-prod",
			"spec": {"api": "responses"},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "sk-test"}
			}]
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

	// The decoder is bound to the deployment ID: the identity check
	// passes for entries naming "ds-prod".
	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "ds-prod",
		ID:       extensionGenerate,
	}}, map[string]inference.ExtensionDecoder{
		"ds-prod/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*GenerateOptions); options.ProviderID() != "ds-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "ds-prod")
	}

	// Entries naming the driver default (or any other ID) stay
	// unregistered instead of silently decoding with a mismatch.
	_, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "deepseek",
		ID:       extensionGenerate,
	}}, map[string]inference.ExtensionDecoder{
		"ds-prod/" + extensionGenerate: decoder,
	}, "extensions")
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "not registered by the host") {
		t.Fatalf("wrong-provider error = %v, want Validation for unregistered identity", err)
	}
}
