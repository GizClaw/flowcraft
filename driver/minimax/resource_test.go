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

func TestProviderCarriesMusicOptionsDecoder(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "minimax",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	decoder, ok := provider.ExtensionDecoders[extensionMusic]
	if !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, extensionMusic)
	}

	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "minimax",
		ID:       extensionMusic,
		Fields:   json.RawMessage(`{"lyrics":"hello world","instrumental":true}`),
	}}, map[string]inference.ExtensionDecoder{
		"minimax/" + extensionMusic: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options := extensions[0].(*MusicOptions)
	if options.ProviderID() != "minimax" || options.Lyrics != "hello world" ||
		options.Instrumental == nil || !*options.Instrumental {
		t.Fatalf("decoded options = %#v", options)
	}

	value, err = Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "mm-prod",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decoder = value.(inference.ProviderDefinition).ExtensionDecoders[extensionMusic]
	extensions, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "mm-prod",
		ID:       extensionMusic,
	}}, map[string]inference.ExtensionDecoder{
		"mm-prod/" + extensionMusic: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*MusicOptions); options.ProviderID() != "mm-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "mm-prod")
	}
}

func TestProviderCarriesVideoOptionsDecoder(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "minimax",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	decoder, ok := provider.ExtensionDecoders[extensionVideo]
	if !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, extensionVideo)
	}

	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "minimax",
		ID:       extensionVideo,
		Fields: json.RawMessage(
			`{"callback_url":"https://example.com/cb","prompt_optimizer":false,"last_frame_only":true}`,
		),
	}}, map[string]inference.ExtensionDecoder{
		"minimax/" + extensionVideo: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options := extensions[0].(*VideoOptions)
	if options.ProviderID() != "minimax" ||
		options.CallbackURL != "https://example.com/cb" ||
		options.PromptOptimizer == nil || *options.PromptOptimizer ||
		options.LastFrameOnly == nil || !*options.LastFrameOnly {
		t.Fatalf("decoded options = %#v", options)
	}
}

func TestProviderCarriesContextIROptionsDecoder(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "minimax",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	decoder, ok := provider.ExtensionDecoders[extensionContextIR]
	if !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q",
			provider.ExtensionDecoders, extensionContextIR)
	}

	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "minimax",
		ID:       extensionContextIR,
		Fields: json.RawMessage(
			`{"duration_millis":5000,"ratio":"16:9","callback_url":"https://example.com/cb"}`,
		),
	}}, map[string]inference.ExtensionDecoder{
		"minimax/" + extensionContextIR: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options := extensions[0].(*ContextIROptions)
	if options.ProviderID() != "minimax" ||
		options.DurationMillis == nil || *options.DurationMillis != 5_000 ||
		options.Ratio != "16:9" ||
		options.CallbackURL != "https://example.com/cb" {
		t.Fatalf("decoded options = %#v", options)
	}
}
