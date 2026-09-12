package route

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestRouterFallsBackOnDeclaredOutputLimit is the end-to-end pin for the
// declaration check: a request whose output budget exceeds the selected
// model's declared limit is rejected before that model is opened, and the
// router falls back to a target that declares room for it. The rejection
// therefore costs no provider work on the too-small target and no caller-visible
// failure when a bigger target exists.
func TestRouterFallsBackOnDeclaredOutputLimit(t *testing.T) {
	driver, err := inference.BindGenerate(
		routeCompiler(),
		routeTransport(false),
		routeDecode(),
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	small := model.ModelRef{ID: model.ModelID{Provider: "small", Name: "model-1"}}
	large := model.ModelRef{ID: model.ModelID{Provider: "large", Name: "model-1"}}

	var smallOpens, largeOpens atomic.Int64
	provider := func(id string, maxOutputTokens int, opens *atomic.Int64) inference.ProviderDefinition {
		return inference.ProviderDefinition{
			ID: id,
			Models: []inference.ModelImplementation{{
				Descriptor: model.ModelDescriptor{
					ID:     model.ModelID{Provider: id, Name: "model-1"},
					Limits: model.ModelLimits{}.WithMaxOutputTokens(maxOutputTokens),
				},
				Openers: inference.Openers{
					Generate: func(
						context.Context, model.ModelRef,
					) (inference.GenerateOperations, error) {
						opens.Add(1)
						return inference.GenerateOperations{Unary: driver}, nil
					},
				},
			}},
		}
	}
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.small": provider("small", 100, &smallOpens),
		"provider.large": provider("large", 1_000, &largeOpens),
	})
	policy := Policy{Generate: []Pool{
		{Tier: "small", Targets: []Target{{Model: small}}},
		{Tier: "large", Targets: []Target{{Model: large}}},
	}}
	router, err := New(assembly, policy.Selectors(assembly))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	requested := 500
	request := routeRequest()
	request.Input.Content.Intent.Text.MaxOutputTokens = &requested
	request.Input.Content.Parts = []message.Part{message.TextPart{Text: "hi"}}

	response, trace, err := router.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("FinishReason = %q, want %q", response.FinishReason, inference.FinishCompleted)
	}
	if smallOpens.Load() != 0 {
		t.Fatalf("too-small target was opened %d times", smallOpens.Load())
	}
	if largeOpens.Load() == 0 {
		t.Fatal("fallback target was never opened")
	}
	if trace.Decision.Selected != small {
		t.Fatalf("selected = %+v, want the small target", trace.Decision.Selected)
	}
	if len(trace.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v, want one hop", trace.Fallbacks)
	}
	if hop := trace.Fallbacks[0]; hop.From != small || hop.To != large ||
		hop.Reason != string(inference.UnsupportedFeature) {
		t.Fatalf("fallback hop = %+v", hop)
	}
	if trace.Executed != large {
		t.Fatalf("executed = %+v, want the large target", trace.Executed)
	}
}
