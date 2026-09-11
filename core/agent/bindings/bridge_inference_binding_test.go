package bindings

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

// countingBridgeAssembly builds a one-model generate assembly whose opener
// counts, so the bridge's reuse of bound drivers is observable.
func countingBridgeAssembly(t *testing.T, opens *atomic.Int64) (*inference.Assembly, map[string]any) {
	t.Helper()
	compile := inference.GenerateCompiler[string](
		func(
			_ context.Context,
			_ model.ModelRef,
			request inference.GenerateRequest,
			shape inference.GenerateExecutionShape,
		) (inference.Compiled[string], error) {
			fields := request.ActiveFieldsFor(shape)
			decisions := make([]inference.Decision, len(fields))
			for index, field := range fields {
				decisions[index] = inference.Decision{
					Field: field, Disposition: inference.Native,
				}
			}
			return inference.Compiled[string]{
				Wire: "wire",
				Report: inference.CompileReport{
					Operation: model.OperationGenerate,
					Decisions: decisions,
				},
			}, nil
		},
	)
	transport := inference.Transport[string, string](
		func(context.Context, string) (string, error) { return "raw", nil },
	)
	decode := inference.Decoder[string, inference.GenerateResponse](
		func(context.Context, string) (inference.GenerateResponse, error) {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{
						Parts: []message.Part{message.TextPart{Text: "ok"}},
					},
				},
				FinishReason: inference.FinishCompleted,
			}, nil
		},
	)
	driver, err := inference.BindGenerate(compile, transport, decode)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	definition := inference.ProviderDefinition{
		ID: "counted",
		Models: []inference.ModelImplementation{{
			Descriptor: model.ModelDescriptor{
				ID: model.ModelID{Provider: "counted", Name: "model-1"},
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
	value, err := inference.Factory{}.New(context.Background(), resource.Input{
		Deps: map[string]any{"provider.counted": definition},
	})
	if err != nil {
		t.Fatalf("build assembly: %v", err)
	}
	return value.(*inference.Assembly), map[string]any{
		"id": map[string]any{"provider": "counted", "name": "model-1"},
	}
}

// TestInferenceBridge_ReusesBoundModel pins the bridge side of the binding
// cache: script calls that keep addressing the same model open it once, while
// each call still compiles and executes its own request.
func TestInferenceBridge_ReusesBoundModel(t *testing.T) {
	var opens atomic.Int64
	assembly, modelJSON := countingBridgeAssembly(t, &opens)
	api := newInferenceAPI(t, assembly, nil)
	for turn := 1; turn <= 3; turn++ {
		out, err := api.generate(map[string]any{
			"model": modelJSON,
			"input": userInput("hi"),
		})
		if err != nil {
			t.Fatalf("turn %d: generate: %v", turn, err)
		}
		if _, ok := out.(map[string]any); !ok {
			t.Fatalf("turn %d: response = %T, want object", turn, out)
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("model opened %d times across 3 script calls, want 1", got)
	}
}
