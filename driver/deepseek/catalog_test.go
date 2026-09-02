package deepseek

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "deepseek"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	for _, model := range provider.Models {
		name := model.Descriptor.ID.Name
		if catalog[name].maxInputTokens <= 0 {
			t.Errorf("model %q: max input tokens not declared", name)
		}
		if model.Descriptor.Limits.MaxInputTokens == nil ||
			*model.Descriptor.Limits.MaxInputTokens != 1_000_000 {
			t.Errorf("model %q: max input tokens = %v, want 1000000",
				name, model.Descriptor.Limits.MaxInputTokens)
		}
	}
}

func TestCatalogPublishesCapabilities(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "deepseek"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	for _, model := range provider.Models {
		capabilities := model.Descriptor.Capabilities
		if !reflect.DeepEqual(capabilities.Outputs, []message.PartKind{message.PartText}) {
			t.Fatalf("%s outputs = %v, want text", model.Descriptor.ID.Name, capabilities.Outputs)
		}
		if !slices.Contains(capabilities.Inputs, message.PartToolCall) {
			t.Fatalf("%s inputs = %v, want tool input", model.Descriptor.ID.Name, capabilities.Inputs)
		}
		if !capabilities.HostedWebSearch ||
			capabilities.Reasoning.Kind != inference.ReasoningToggle {
			t.Fatalf("%s capabilities = %+v", model.Descriptor.ID.Name, capabilities)
		}
	}
}

func TestVisionCatalogDeclaresImageInput(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "deepseek"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	for _, model := range provider.Models {
		if model.Descriptor.ID.Name != "deepseek-v4-flash-vision-exp" {
			continue
		}
		if !slices.Contains(
			model.Descriptor.Capabilities.Inputs,
			message.PartImage,
		) {
			t.Fatalf(
				"vision model inputs = %v, want image input",
				model.Descriptor.Capabilities.Inputs,
			)
		}
		if !model.Descriptor.Capabilities.HostedWebSearch ||
			model.Descriptor.Capabilities.Reasoning.Kind != inference.ReasoningToggle {
			t.Fatalf(
				"vision model capabilities = %+v",
				model.Descriptor.Capabilities,
			)
		}
		return
	}
	t.Fatal("vision model missing from provider catalog")
}

func TestMergedCatalogRejectsMissingTextOutput(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"models":[{"name":"m","kind":"generate"}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if _, err := mergedCatalog(spec); err == nil {
		t.Fatal("mergedCatalog unexpectedly accepted a generate model without text output")
	}
}

func TestRequestMetadataEnvelopeValidationAndCatalogPropagation(t *testing.T) {
	for _, envelope := range []string{"", "metadata", "client_metadata", "request_fields"} {
		raw := `{}`
		if envelope != "" {
			raw = fmt.Sprintf(`{"request_metadata":{"envelope":%q}}`, envelope)
		}
		spec, err := decodeSpec(context.Background(), []byte(raw))
		if err != nil {
			t.Fatalf("decodeSpec(envelope %q): %v", envelope, err)
		}
		models, err := mergedCatalog(spec)
		if err != nil {
			t.Fatalf("mergedCatalog(envelope %q): %v", envelope, err)
		}
		if got := models["deepseek-v4-flash"].requestMetadataEnvelope; got != envelope {
			t.Fatalf(
				"catalog envelope = %q, want %q",
				got,
				envelope,
			)
		}
	}
}
