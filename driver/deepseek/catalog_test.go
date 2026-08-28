package deepseek

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestCatalogDeclaresMaxInputTokens(t *testing.T) {
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "deepseek"})
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
	provider, err := buildProvider(context.Background(), ResourceSettings{ID: "deepseek"})
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
			capabilities.Reasoning != inference.ReasoningToggle {
			t.Fatalf("%s capabilities = %+v", model.Descriptor.ID.Name, capabilities)
		}
	}
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
