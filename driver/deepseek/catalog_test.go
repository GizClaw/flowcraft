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
		if catalog[name].limits.MaxInputTokens == nil {
			t.Errorf("model %q: max input tokens not declared", name)
		}
		if model.Descriptor.Limits.MaxInputTokens == nil ||
			*model.Descriptor.Limits.MaxInputTokens != 1_000_000 {
			t.Errorf("model %q: max input tokens = %v, want 1000000",
				name, model.Descriptor.Limits.MaxInputTokens)
		}
	}
}

func TestCatalogDeclaresMaxOutputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "deepseek"}, nil)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	for _, model := range provider.Models {
		name := model.Descriptor.ID.Name
		if catalog[name].limits.MaxOutputTokens == nil {
			t.Errorf("model %q: max output tokens not declared", name)
		}
		if model.Descriptor.Limits.MaxOutputTokens == nil ||
			*model.Descriptor.Limits.MaxOutputTokens != 384_000 {
			t.Errorf("model %q: max output tokens = %v, want 384000",
				name, model.Descriptor.Limits.MaxOutputTokens)
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

func TestMergedCatalogOverlaysDeclaredLimits(t *testing.T) {
	capabilities := `{"inputs":["text","data","tool_call","tool_result"],"outputs":["text"]}`
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "deepseek-v4-flash",
			"capabilities": `+capabilities+`
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["deepseek-v4-flash"]
	in, out := entry.limits.Values()
	if in != 1_000_000 || out != 384_000 {
		t.Fatalf("redeclared limits = %d/%d, want catalog 1000000/384000",
			in, out)
	}

	spec, err = decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "deepseek-v4-flash",
			"capabilities": `+capabilities+`,
			"limits": {"max_output_tokens": 64000}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err = mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry = models["deepseek-v4-flash"]
	in, out = entry.limits.Values()
	if in != 1_000_000 || out != 64_000 {
		t.Fatalf("overridden limits = %d/%d, want 1000000/64000",
			in, out)
	}
}

func TestMergedCatalogOverridesDeclaredInputLimit(t *testing.T) {
	capabilities := `{"inputs":["text","data","tool_call","tool_result"],"outputs":["text"]}`
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{
			"name": "deepseek-v4-flash",
			"capabilities": `+capabilities+`,
			"limits": {"max_input_tokens": 65536}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	entry := models["deepseek-v4-flash"]
	in, out := entry.limits.Values()
	if in != 65_536 || out != 384_000 {
		t.Fatalf("declared input limits = %d/%d, want 65536/384000",
			in, out)
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
