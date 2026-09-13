package inference

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// declarationAssembly builds a one-model generate provider whose descriptor
// declares limits, counting how often the opener runs.
func declarationAssembly(
	t *testing.T,
	limits ModelLimits,
	opens *atomic.Int64,
) *Assembly {
	t.Helper()
	return declarationAssemblyFor(t, ModelDescriptor{
		ID:     ModelID{Provider: "limited", Name: "model-1"},
		Limits: limits,
	}, opens)
}

// declarationAssemblyFor builds the same one-model provider for an explicit
// descriptor, so a test can declare capabilities as well as limits.
func declarationAssemblyFor(
	t *testing.T,
	descriptor ModelDescriptor,
	opens *atomic.Int64,
) *Assembly {
	t.Helper()
	return &Assembly{providers: map[string]ProviderDefinition{
		"limited": {
			ID: "limited",
			Models: []ModelImplementation{{
				Descriptor: descriptor,
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

// generateRequestWithParts is a plain text request whose current input carries
// extra content parts.
func generateRequestWithParts(parts ...message.Part) GenerateRequest {
	request := generateRequestWithMaxOutput(0)
	request.Input.Content.Intent.Text.MaxOutputTokens = nil
	request.Input.Content.Parts = append(
		[]message.Part{message.TextPart{Text: "hi"}},
		parts...,
	)
	return request
}

// imagePart is one URL-backed image a request can carry.
func imagePart(t *testing.T) message.Part {
	t.Helper()
	source, err := media.NewImageURL("https://cdn.example.com/shot.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return message.ImagePart{Source: source}
}

// TestGenerateRejectsUndeclaredInputKind pins the input half of the
// declaration check: a request carrying a content kind the model does not
// declare is rejected before the driver is opened — the same decision the
// driver's compiler would make, but without constructing a provider client
// first — with a field and detail the report can name.
func TestGenerateRejectsUndeclaredInputKind(t *testing.T) {
	var opens atomic.Int64
	assembly := declarationAssemblyFor(t, ModelDescriptor{
		ID: ModelID{Provider: "limited", Name: "model-1"},
		Capabilities: ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
		},
	}, &opens)

	_, err := assembly.Generate(
		context.Background(), declarationModel,
		generateRequestWithParts(imagePart(t)),
	)
	if err == nil {
		t.Fatal("Generate accepted an undeclared image input")
	}
	if !IsKind(err, UnsupportedFeature) {
		t.Fatalf("kind = %v, want %v", err, UnsupportedFeature)
	}
	var inferenceErr *Error
	if !errors.As(err, &inferenceErr) {
		t.Fatalf("error is not an *Error: %v", err)
	}
	if inferenceErr.Field != FieldGenerateInputImage {
		t.Fatalf("field = %q, want %q", inferenceErr.Field, FieldGenerateInputImage)
	}
	if want := declarationDetailInputPrefix + string(message.PartImage); inferenceErr.Detail != want {
		t.Fatalf("detail = %q, want %q", inferenceErr.Detail, want)
	}
	if opens.Load() != 0 {
		t.Fatalf("driver opened %d times for a request rejected on declaration", opens.Load())
	}
}

// TestGenerateInputDeclarationBoundaries pins exactly which requests the input
// check does and does not reject, including the two deliberate exceptions.
func TestGenerateInputDeclarationBoundaries(t *testing.T) {
	toolResult := message.ToolResultPart{Result: message.NewTextToolResult(
		"c1", "tool output",
	)}
	cases := []struct {
		name     string
		declared []message.PartKind
		parts    []message.Part
		context  []message.Part
		rejected bool
		field    FieldID
	}{
		{
			name:     "declared image passes",
			declared: []message.PartKind{message.PartText, message.PartImage},
			parts:    []message.Part{imagePart(t)},
		},
		{
			name:     "undeclared image is rejected",
			declared: []message.PartKind{message.PartText},
			parts:    []message.Part{imagePart(t)},
			rejected: true,
			field:    FieldGenerateInputImage,
		},
		{
			name:     "context image is rejected with the context field",
			declared: []message.PartKind{message.PartText},
			context:  []message.Part{imagePart(t)},
			rejected: true,
			field:    FieldGenerateContextImage,
		},
		{
			name:  "undeclared list is unknown, not empty",
			parts: []message.Part{imagePart(t)},
		},
		{
			// Text is the baseline every compiler lowers; a declaration that
			// omits it does not turn text requests into rejections.
			name:     "text is never gated",
			declared: []message.PartKind{message.PartImage},
		},
		{
			// Structural parts keep their role and surface rules in the
			// driver, which may drop rather than reject them.
			name:     "tool results stay with the compiler",
			declared: []message.PartKind{message.PartText},
			parts:    []message.Part{toolResult},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opens atomic.Int64
			assembly := declarationAssemblyFor(t, ModelDescriptor{
				ID:           ModelID{Provider: "limited", Name: "model-1"},
				Capabilities: ModelCapabilities{Inputs: tc.declared},
			}, &opens)
			request := generateRequestWithParts(tc.parts...)
			if len(tc.context) > 0 {
				request.Context = []message.Message{{
					Role:    message.RoleUser,
					Content: message.Content{Parts: tc.context},
				}}
			}
			_, err := assembly.Generate(
				context.Background(), declarationModel, request,
			)
			switch {
			case tc.rejected && err == nil:
				t.Fatal("Generate accepted the request, want a declaration rejection")
			case !tc.rejected && err != nil:
				t.Fatalf("Generate: %v", err)
			}
			if !tc.rejected {
				if opens.Load() == 0 {
					t.Fatal("the driver was never opened for an accepted request")
				}
				return
			}
			var inferenceErr *Error
			if !errors.As(err, &inferenceErr) {
				t.Fatalf("error is not an *Error: %v", err)
			}
			if inferenceErr.Field != tc.field {
				t.Fatalf("field = %q, want %q", inferenceErr.Field, tc.field)
			}
			if opens.Load() != 0 {
				t.Fatalf("driver opened %d times, want none", opens.Load())
			}
		})
	}
}

// TestBindingPrepareRejectsUndeclaredInputKind pins the second entry point: a
// binding applies the same declaration check when it compiles, so a prepared
// attempt never reaches a driver that would reject it.
func TestBindingPrepareRejectsUndeclaredInputKind(t *testing.T) {
	var opens atomic.Int64
	assembly := declarationAssemblyFor(t, ModelDescriptor{
		ID: ModelID{Provider: "limited", Name: "model-1"},
		Capabilities: ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
		},
	}, &opens)
	binding, err := assembly.Bind(context.Background(), declarationModel)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	_, err = binding.PrepareGenerate(
		context.Background(), generateRequestWithParts(imagePart(t)),
	)
	if err == nil {
		t.Fatal("PrepareGenerate accepted an undeclared image input")
	}
	var inferenceErr *Error
	if !errors.As(err, &inferenceErr) ||
		inferenceErr.Field != FieldGenerateInputImage {
		t.Fatalf("error = %v, want a declaration rejection naming the image field", err)
	}
}
