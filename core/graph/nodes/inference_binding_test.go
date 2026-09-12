package nodes

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

// countingAssembly builds a one-model generate assembly whose opener and
// compiler are counted, so the node's use of the binding cache is observable.
func countingAssembly(
	t *testing.T,
	opens, compiles *atomic.Int64,
) (*inference.Assembly, model.ModelRef) {
	t.Helper()
	compile := inference.GenerateCompiler[string](
		func(
			_ context.Context,
			_ model.ModelRef,
			request inference.GenerateRequest,
			shape inference.GenerateExecutionShape,
		) (inference.Compiled[string], error) {
			compiles.Add(1)
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
	ref := model.ModelRef{ID: model.ModelID{Provider: "counted", Name: "model-1"}}
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
	return value.(*inference.Assembly), ref
}

// TestInferenceNode_ReusesBoundModelAcrossTurns pins the host side of the
// binding API: the node opens its configured model once and reuses the drivers
// for every later turn, while still compiling each request on its own.
func TestInferenceNode_ReusesBoundModelAcrossTurns(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly, ref := countingAssembly(t, &opens, &compiles)
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: assembly})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:     ptr(ref),
		OutputKey: "answer",
	})
	board := userBoard()
	for turn := 1; turn <= 3; turn++ {
		if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
			t.Fatalf("turn %d: execute: %v", turn, err)
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleUser, "again"))
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("model opened %d times across %d turns, want 1", got, 3)
	}
	if got := compiles.Load(); got != 3 {
		t.Fatalf("compiles across %d turns = %d, want one per turn", 3, got)
	}
}

// TestInferenceNode_BindsEachModelOnce pins the cache key: a second call for
// the same model reuses the binding rather than reopening it, and an
// unresolvable model fails without disturbing the cached one.
func TestInferenceNode_BindsEachModelOnce(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly, ref := countingAssembly(t, &opens, &compiles)
	if err := assembly.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cache := inference.NewBindingCache(assembly)
	ctx := context.Background()

	first, err := cache.Bind(ctx, ref)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	again, err := cache.Bind(ctx, ref)
	if err != nil {
		t.Fatalf("Bind again: %v", err)
	}
	if first != again {
		t.Fatal("the same model reference produced two bindings")
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens = %d, want 1", got)
	}
	if _, err := cache.Bind(ctx, model.ModelRef{
		ID: model.ModelID{Provider: "counted", Name: "missing"},
	}); err == nil {
		t.Fatal("binding an unknown model must fail")
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens after a failed bind = %d, want 1", got)
	}
}
