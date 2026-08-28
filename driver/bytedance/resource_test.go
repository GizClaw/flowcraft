package bytedance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProvider(t *testing.T) {
	t.Setenv("BYTEDANCE_TEST_KEY", "sk-test")
	settings, err := resource.Expand(context.Background(),
		json.RawMessage(`{
			"id": "bytedance",
			"spec": {},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "${env:BYTEDANCE_TEST_KEY}"}
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
	if provider.ID != "bytedance" || len(provider.Models) == 0 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestResourceFactoryRejectsRealtime(t *testing.T) {
	_, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "bytedance",
			"spec": {"models": [{"name": "model", "kind": "realtime"}]},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "sk-test"}
			}]
		}`),
	})
	if err == nil {
		t.Fatalf("New accepted kind realtime before core exposes that surface")
	}
}

func TestRegisterAddsByTedanceProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "bytedance"); !ok {
		t.Fatalf("factory %s/bytedance missing", ResourceKind)
	}
}

func TestProviderCarriesExtensionDecoders(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "bytedance",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	decoders := map[string]inference.ExtensionDecoder{}
	for id, decoder := range provider.ExtensionDecoders {
		decoders["bytedance/"+id] = decoder
	}
	for _, id := range []string{extensionGenerate, extensionImage, extensionVideo} {
		if _, ok := decoders["bytedance/"+id]; !ok {
			t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, id)
		}
	}

	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{
		{
			Provider: "bytedance",
			ID:       extensionGenerate,
			Fields:   json.RawMessage(`{"service_tier":"auto","web_search":{"limit":3}}`),
		},
		{
			Provider: "bytedance",
			ID:       extensionImage,
			Fields:   json.RawMessage(`{"watermark":true,"size_token":"1k"}`),
		},
		{
			Provider: "bytedance",
			ID:       extensionVideo,
			Fields:   json.RawMessage(`{"camera_fixed":true,"generate_audio":true}`),
		},
	}, decoders, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	generate, ok := extensions[0].(*GenerateOptions)
	if !ok || generate.ProviderID() != "bytedance" || generate.ServiceTier != "auto" ||
		generate.WebSearch == nil || generate.WebSearch.Limit == nil ||
		*generate.WebSearch.Limit != 3 {
		t.Fatalf("decoded generate options = %#v", extensions[0])
	}
	image, ok := extensions[1].(*ImageOptions)
	if !ok || image.ProviderID() != "bytedance" || image.Watermark == nil ||
		!*image.Watermark || image.SizeToken != "1k" {
		t.Fatalf("decoded image options = %#v", extensions[1])
	}
	video, ok := extensions[2].(*VideoOptions)
	if !ok || video.ProviderID() != "bytedance" || video.CameraFixed == nil ||
		!*video.CameraFixed || video.GenerateAudio == nil || !*video.GenerateAudio {
		t.Fatalf("decoded video options = %#v", extensions[2])
	}

	value, err = Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "bd-prod",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decoder := value.(inference.ProviderDefinition).ExtensionDecoders[extensionGenerate]
	extensions, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "bd-prod",
		ID:       extensionGenerate,
	}}, map[string]inference.ExtensionDecoder{
		"bd-prod/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*GenerateOptions); options.ProviderID() != "bd-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "bd-prod")
	}
}
