package inference

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// declarationAssembly builds a one-model generate provider whose descriptor
// declares limits, counting how often the opener runs.
func declarationAssembly(
	t *testing.T,
	limits ModelLimits,
	opens *atomic.Int64,
) *Assembly {
	t.Helper()
	return &Assembly{providers: map[string]ProviderDefinition{
		"limited": {
			ID: "limited",
			Models: []ModelImplementation{{
				Descriptor: ModelDescriptor{
					ID:     ModelID{Provider: "limited", Name: "model-1"},
					Limits: limits,
				},
				Openers: Openers{
					Generate: func(
						context.Context, ModelRef,
					) (GenerateOperations, error) {
						opens.Add(1)
						return GenerateOperations{Unary: stubGenerateDriver{
							resp: GenerateResponse{Usage: Usage{TotalTokens: 1}},
						}}, nil
					},
				},
			}},
		},
	}}
}

var declarationModel = ModelRef{
	ID: ModelID{Provider: "limited", Name: "model-1"},
}

func generateRequestWithMaxOutput(tokens int) GenerateRequest {
	return GenerateRequest{Input: GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
			Intent:  Intent{Text: &TextIntent{MaxOutputTokens: &tokens}},
		},
	}}
}

// TestGenerateRejectsRequestAboveDeclaredOutputLimit pins the declaration
// check: an over-limit request is rejected before the driver is opened, with a
// kind the route layer treats as transport-safe.
func TestGenerateRejectsRequestAboveDeclaredOutputLimit(t *testing.T) {
	var opens atomic.Int64
	assembly := declarationAssembly(t, ModelLimits{}.WithMaxOutputTokens(100), &opens)

	_, err := assembly.Generate(
		context.Background(), declarationModel, generateRequestWithMaxOutput(101),
	)
	if err == nil {
		t.Fatal("Generate accepted a request above the declared output limit")
	}
	if !IsKind(err, UnsupportedFeature) {
		t.Fatalf("kind = %v, want %v", err, UnsupportedFeature)
	}
	var inferenceErr *Error
	if !errors.As(err, &inferenceErr) {
		t.Fatalf("error is not an *Error: %v", err)
	}
	if inferenceErr.Field != FieldGenerateIntentTextMaxOutputTokens {
		t.Fatalf("field = %q, want %q",
			inferenceErr.Field, FieldGenerateIntentTextMaxOutputTokens)
	}
	if inferenceErr.Detail != declarationDetailMaxOutputTokens {
		t.Fatalf("detail = %q, want %q",
			inferenceErr.Detail, declarationDetailMaxOutputTokens)
	}
	if opens.Load() != 0 {
		t.Fatalf("driver opened %d times for a request rejected on declaration", opens.Load())
	}
}

// TestGenerateDeclarationCheckBoundaries pins where the check does and does
// not fire: at the limit and below pass, an undeclared limit passes, and both
// the explain and stream paths reject the same request as unary generate.
func TestGenerateDeclarationCheckBoundaries(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		limits    ModelLimits
		requested int
		rejected  bool
	}{
		{name: "below limit", limits: ModelLimits{}.WithMaxOutputTokens(100), requested: 99},
		{name: "at limit", limits: ModelLimits{}.WithMaxOutputTokens(100), requested: 100},
		{name: "above limit", limits: ModelLimits{}.WithMaxOutputTokens(100), requested: 101, rejected: true},
		{name: "undeclared limit", limits: ModelLimits{}, requested: 1_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opens atomic.Int64
			assembly := declarationAssembly(t, tc.limits, &opens)
			request := generateRequestWithMaxOutput(tc.requested)

			_, err := assembly.Generate(ctx, declarationModel, request)
			if tc.rejected != IsKind(err, UnsupportedFeature) {
				t.Fatalf("Generate err = %v, rejected = %v", err, tc.rejected)
			}
			if _, err := assembly.ExplainGenerate(ctx, declarationModel, request); tc.rejected != IsKind(err, UnsupportedFeature) {
				t.Fatalf("ExplainGenerate err = %v, rejected = %v", err, tc.rejected)
			}
			if _, err := assembly.GenerateStream(ctx, declarationModel, request); tc.rejected != IsKind(err, UnsupportedFeature) {
				t.Fatalf("GenerateStream err = %v, rejected = %v", err, tc.rejected)
			}
			if _, err := assembly.ExplainGenerateStream(ctx, declarationModel, request); tc.rejected != IsKind(err, UnsupportedFeature) {
				t.Fatalf("ExplainGenerateStream err = %v, rejected = %v", err, tc.rejected)
			}
			wantOpens := int64(0)
			if !tc.rejected {
				// All four generate entry points resolve the driver before
				// discovering that this model serves no stream.
				wantOpens = 4
			}
			if opens.Load() != wantOpens {
				t.Fatalf("opener ran %d times, want %d", opens.Load(), wantOpens)
			}
		})
	}
}

// TestGenerateWithoutTextIntentSkipsDeclarationCheck pins that the check is
// scoped to the text intent that carries the budget.
func TestGenerateWithoutTextIntentSkipsDeclarationCheck(t *testing.T) {
	var opens atomic.Int64
	assembly := declarationAssembly(t, ModelLimits{}.WithMaxOutputTokens(100), &opens)
	request := GenerateRequest{Input: GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
			Intent:  Intent{Image: &ImageIntent{}},
		},
	}}
	if _, err := assembly.Generate(context.Background(), declarationModel, request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}
