package inference

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestPreparedExecuteIsOneProviderRoundTrip pins the documented semantics of a
// prepared attempt: the compilation is reused, the call is not. Executing one
// handle twice reaches the provider twice, which is what makes "retry the
// compiled attempt" possible and what a caller must not do by accident.
func TestPreparedExecuteIsOneProviderRoundTrip(t *testing.T) {
	var opens, compiles, transports atomic.Int64
	compile := GenerateCompiler[string](
		func(
			_ context.Context,
			_ ModelRef,
			request GenerateRequest,
			shape GenerateExecutionShape,
		) (Compiled[string], error) {
			compiles.Add(1)
			fields := request.ActiveFieldsFor(shape)
			decisions := make([]Decision, len(fields))
			for index, field := range fields {
				decisions[index] = Decision{Field: field, Disposition: Native}
			}
			return Compiled[string]{
				Wire: "wire",
				Report: CompileReport{
					Operation: OperationGenerate,
					Decisions: decisions,
				},
			}, nil
		},
	)
	transport := Transport[string, string](
		func(context.Context, string) (string, error) {
			transports.Add(1)
			return "raw", nil
		},
	)
	decode := Decoder[string, GenerateResponse](
		func(context.Context, string) (GenerateResponse, error) {
			return generateResponseForTest(), nil
		},
	)
	assembly := preparedTestAssembly(t, compile, transport, decode, &opens)

	prepared, err := assembly.PrepareGenerate(
		context.Background(), bindingRef, bindingRequest())
	if err != nil {
		t.Fatalf("PrepareGenerate: %v", err)
	}
	if got := compiles.Load(); got != 1 {
		t.Fatalf("compiles after preparing = %d, want 1", got)
	}
	for round := 1; round <= 2; round++ {
		if _, err := prepared.Execute(context.Background()); err != nil {
			t.Fatalf("execute %d: %v", round, err)
		}
	}
	if got := compiles.Load(); got != 1 {
		t.Fatalf("compiles after two executes = %d, want 1 (compilation is reused)", got)
	}
	if got := transports.Load(); got != 2 {
		t.Fatalf("provider calls after two executes = %d, want 2", got)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens = %d, want 1", got)
	}
}

// TestPreparedExplanationIsASnapshot pins that reading the compiler decisions
// does not consume or change the handle.
func TestPreparedExplanationIsASnapshot(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles, nil)
	prepared, err := assembly.PrepareGenerate(
		context.Background(), bindingRef, bindingRequest())
	if err != nil {
		t.Fatalf("PrepareGenerate: %v", err)
	}
	first := prepared.Explanation()
	first.Decisions[0] = Decision{Field: "rewritten", Disposition: Dropped, Reason: "test"}
	second := prepared.Explanation()
	if second.Decisions[0].Field == "rewritten" {
		t.Fatal("Explanation shares its decision slice with the caller")
	}
	if _, err := prepared.Execute(context.Background()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := assembly.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// preparedTestAssembly builds a one-model generate assembly over an explicit
// pipeline, counting opens.
func preparedTestAssembly(
	t *testing.T,
	compile GenerateCompiler[string],
	transport Transport[string, string],
	decode Decoder[string, GenerateResponse],
	opens *atomic.Int64,
) *Assembly {
	t.Helper()
	driver, err := BindGenerate(compile, transport, decode)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	return &Assembly{providers: map[string]ProviderDefinition{
		"binding": {
			ID: "binding",
			Models: []ModelImplementation{{
				Descriptor: ModelDescriptor{
					ID: ModelID{Provider: "binding", Name: "model-1"},
				},
				Openers: Openers{
					Generate: func(
						context.Context, ModelRef,
					) (GenerateOperations, error) {
						opens.Add(1)
						return GenerateOperations{Unary: driver}, nil
					},
				},
			}},
		},
	}}
}

func generateResponseForTest() GenerateResponse {
	return GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{
				Parts: []message.Part{message.TextPart{Text: "ok"}},
			},
		},
		FinishReason: FinishCompleted,
		Usage:        Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}
}
