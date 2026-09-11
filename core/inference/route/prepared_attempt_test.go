package route

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// countingCompiler mirrors routeCompiler but counts how often the compiler ran,
// so the attempt engine's "compile once" contract is observable.
func countingCompiler(compiles *atomic.Int64) inference.GenerateCompiler[routeWire] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[routeWire], error) {
		compiles.Add(1)
		active := request.ActiveFieldsFor(shape)
		decisions := make([]inference.Decision, len(active))
		for index, field := range active {
			decisions[index] = inference.Decision{
				Field: field, Disposition: inference.Native,
			}
		}
		return inference.Compiled[routeWire]{
			Wire: routeWire{},
			Report: inference.CompileReport{
				Operation: model.OperationGenerate,
				Decisions: decisions,
			},
		}, nil
	}
}

// TestRouterCompilesOncePerAttempt pins the P3 win end to end: the router's
// preflight and its work share one compilation, so a routed generate call
// compiles once instead of twice.
func TestRouterCompilesOncePerAttempt(t *testing.T) {
	var opens, compiles atomic.Int64
	driver, err := inference.BindGenerate(
		countingCompiler(&compiles),
		routeTransport(false),
		routeDecode(),
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.counted": {
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
		},
	})
	policy := Policy{Generate: []Pool{{
		Tier: "fast",
		Targets: []Target{{
			Model: model.ModelRef{
				ID: model.ModelID{Provider: "counted", Name: "model-1"},
			},
		}},
	}}}
	router, err := New(assembly, policy.Selectors(assembly))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := router.Generate(context.Background(), routeRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := compiles.Load(); got != 1 {
		t.Fatalf("compiles per routed attempt = %d, want 1", got)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens per routed attempt = %d, want 1", got)
	}
}
