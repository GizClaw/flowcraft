package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProvider(t *testing.T) {
	t.Setenv("AZURE_TEST_KEY", "sk-test")
	settings, err := resource.Expand(context.Background(),
		json.RawMessage(`{
			"id": "azure",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{"name": "gpt-5", "kind": "generate", "capabilities": {"outputs": ["text"]}}]
			},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "${env:AZURE_TEST_KEY}"}
			}]
		}`), resource.ExpandEnv())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	value, err := Factory().New(context.Background(), resource.Input{Settings: settings})
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

func TestProviderCarriesGenerateOptionsDecoder(t *testing.T) {
	settings, err := resource.Expand(context.Background(),
		json.RawMessage(`{
			"id": "azure",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{"name": "gpt-5", "kind": "generate", "capabilities": {"outputs": ["text"]}}]
			},
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`), resource.ExpandEnv())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	value, err := Factory().New(context.Background(), resource.Input{Settings: settings})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	decoder, ok := provider.ExtensionDecoders[extensionGenerate]
	if !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, extensionGenerate)
	}

	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "azure",
		ID:       extensionGenerate,
		Fields: json.RawMessage(`{
			"web_search": {
				"tool_choice": {"required": true},
				"search_context_size": "high"
			}
		}`),
	}}, map[string]inference.ExtensionDecoder{
		"azure/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options := extensions[0].(*GenerateOptions)
	if options.ProviderID() != "azure" || options.WebSearch == nil ||
		options.WebSearch.ToolChoice == nil || !options.WebSearch.ToolChoice.Required ||
		options.WebSearch.SearchContextSize != "high" {
		t.Fatalf("decoded options = %#v", options)
	}

	value, err = Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "az-prod",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{"name": "gpt-5", "kind": "generate", "capabilities": {"outputs": ["text"]}}]
			},
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decoder = value.(inference.ProviderDefinition).ExtensionDecoders[extensionGenerate]
	extensions, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "az-prod",
		ID:       extensionGenerate,
	}}, map[string]inference.ExtensionDecoder{
		"az-prod/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*GenerateOptions); options.ProviderID() != "az-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "az-prod")
	}
}

func TestProviderCarriesImageOptionsDecoder(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "azure",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{"name": "gpt-image-1", "kind": "image", "capabilities": {"outputs": ["image"]}}]
			},
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	decoder, ok := provider.ExtensionDecoders[extensionImage]
	if !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, extensionImage)
	}

	maskPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "azure",
		ID:       extensionImage,
		Fields: json.RawMessage(`{
			"mask": {
				"kind": "inline",
				"data": "` + maskPNG + `",
				"media_type": "image/png"
			}
		}`),
	}}, map[string]inference.ExtensionDecoder{
		"azure/" + extensionImage: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options := extensions[0].(*ImageOptions)
	if options.ProviderID() != "azure" || options.Mask == nil ||
		options.Mask.Kind() != media.SourceInline ||
		!bytes.Equal(options.Mask.Bytes(), testPNG) {
		t.Fatalf("decoded options = %#v", options)
	}

	value, err = Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "az-prod",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{"name": "gpt-image-1", "kind": "image", "capabilities": {"outputs": ["image"]}}]
			},
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decoder = value.(inference.ProviderDefinition).ExtensionDecoders[extensionImage]
	extensions, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "az-prod",
		ID:       extensionImage,
	}}, map[string]inference.ExtensionDecoder{
		"az-prod/" + extensionImage: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*ImageOptions); options.ProviderID() != "az-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "az-prod")
	}
}

func TestModelCapabilitiesPublish(t *testing.T) {
	t.Setenv("AZURE_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "azure",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{
					"name": "gpt-5",
					"kind": "generate",
					"capabilities": {
						"inputs": ["text", "image", "data", "tool_call", "tool_result"],
						"outputs": ["text"],
						"hosted_web_search": true,
						"reasoning": "always"
					}
				}]
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
	provider := value.(inference.ProviderDefinition)
	capabilities := provider.Models[0].Descriptor.Capabilities
	if !reflect.DeepEqual(capabilities.Outputs, []message.PartKind{message.PartText}) ||
		!slices.Contains(capabilities.Inputs, message.PartImage) ||
		!capabilities.HostedWebSearch ||
		capabilities.Reasoning.Kind != inference.ReasoningAlways {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestSpecRejectsFamilyContractViolation(t *testing.T) {
	_, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "azure",
			"spec": {
				"endpoint": "https://example.openai.azure.com",
				"models": [{
					"name": "img",
					"kind": "image",
					"capabilities": {"outputs": ["text"]}
				}]
			},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "sk-test"}
			}]
		}`),
	})
	if err == nil {
		t.Fatal("image deployment with text output unexpectedly accepted")
	}
}
