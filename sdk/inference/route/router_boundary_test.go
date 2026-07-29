package route

import (
	"context"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

type stubRealtimeSession struct{}

func (*stubRealtimeSession) Send(context.Context, string) error { return nil }
func (*stubRealtimeSession) Next(context.Context) (string, error) {
	return "", io.EOF
}
func (*stubRealtimeSession) CancelResponse(context.Context) error { return nil }
func (*stubRealtimeSession) Close() error                         { return nil }

func TestRouterClassifiesUnknownSelectorTargetAsSelectionFailure(t *testing.T) {
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"model": {},
	})
	unknown := generateModel("ghost")
	router := newGenerateRouter(t, runtime, unknown, nil)

	_, _, err := router.Generate(context.Background(), generateRequest("hello"))
	if !IsKind(err, SelectionFailed) {
		t.Fatalf("Generate error = %v, want SelectionFailed", err)
	}
	if !errdefs.IsNotFound(err) {
		t.Fatalf("Generate error = %v, want not-found classification", err)
	}
	if !inference.IsKind(err, inference.UnknownModel) {
		t.Fatalf("Generate error = %v, want unknown model in chain", err)
	}
}

func TestRouterClassifiesInvalidRequestBeforeSelection(t *testing.T) {
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"model": {},
	})
	called := false
	router, err := New(runtime, Selectors{
		Generate: generateSelectorFunc(func(
			context.Context,
			inference.GenerateRequest,
		) (Decision, error) {
			called = true
			return Decision{}, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = router.Generate(context.Background(), inference.GenerateRequest{})
	if !IsKind(err, InvalidRequest) {
		t.Fatalf("Generate error = %v, want InvalidRequest", err)
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("Generate error = %v, want validation classification", err)
	}
	if called {
		t.Fatal("selector invoked for an invalid request")
	}
}

func TestRouterSessionTracesTrustRuntimeTarget(t *testing.T) {
	driver, err := inference.BindRealtime(
		func(
			context.Context,
			inference.ModelRef,
			inference.RealtimeConfig,
		) (inference.Compiled[string], error) {
			return inference.Compiled[string]{
				Wire: "session",
				Report: inference.CompileReport{
					Operation: inference.OperationRealtime,
					Decisions: []inference.Decision{{
						Field:       inference.FieldRealtimeModalities,
						Disposition: inference.Native,
					}},
				},
			}, nil
		},
		func(
			context.Context,
			string,
		) (inference.ProviderRealtimeSession[string, string], error) {
			return &stubRealtimeSession{}, nil
		},
		func(
			context.Context,
			inference.ModelRef,
			inference.RealtimeInput,
		) (inference.Compiled[string], error) {
			return inference.Compiled[string]{}, nil
		},
		func(context.Context, string) (inference.RealtimeEvent, error) {
			return inference.RealtimeTextDeltaEvent{Delta: "ok"}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindRealtime: %v", err)
	}
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "fake",
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "fake", Name: "live"},
			},
			Openers: inference.Openers{
				Realtime: func(
					context.Context,
					inference.ModelRef,
				) (inference.RealtimeDriver, error) {
					return driver, nil
				},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	target := inference.ModelRef{
		ID: inference.ModelID{Provider: "fake", Name: "live"},
	}
	router, err := New(runtime, Selectors{
		Realtime: realtimeSelectorFunc(func(
			context.Context,
			inference.RealtimeConfig,
		) (Decision, error) {
			return Decision{
				Operation: inference.OperationRealtime,
				Tier:      "live",
				Proposed:  target,
				Selected:  target,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, trace, err := router.OpenRealtime(
		context.Background(),
		inference.RealtimeConfig{
			Modalities: []inference.Modality{inference.ModalityText},
		},
	)
	if err != nil {
		t.Fatalf("OpenRealtime: %v", err)
	}
	defer func() { _ = session.Close() }()
	if trace.Executed != target {
		t.Fatalf("session trace executed = %+v, want %+v", trace.Executed, target)
	}
}

func TestTraceCloneOwnsSlices(t *testing.T) {
	target := generateModel("model")
	trace := Trace{
		Decision:  generateDecision(target),
		Executed:  target,
		Fallbacks: []FallbackHop{{From: target, To: target, Reason: "x"}},
		Attempts: []Attempt{{
			Target: target, Phase: AttemptPhaseExecute,
			Trigger: AttemptTriggerSelection, Outcome: AttemptOutcomeSucceeded,
		}},
	}
	clone := trace.Clone()
	clone.Fallbacks[0].Reason = "changed"
	clone.Attempts[0].Outcome = AttemptOutcomeFailed
	if trace.Fallbacks[0].Reason != "x" {
		t.Fatal("trace clone shares fallback slice")
	}
	if trace.Attempts[0].Outcome != AttemptOutcomeSucceeded {
		t.Fatal("trace clone shares attempt slice")
	}
}
