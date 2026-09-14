package bindings

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func testInvocation() Invocation {
	return Invocation{Context: context.Background(), Board: agent.NewBoard()}
}

func TestAssembleRunsLatePhaseAfterOrdinaryWithFinalEnv(t *testing.T) {
	var sawOrdinary bool
	provider := providerWithLate{
		ordinary: []Binding{{Name: "first", Value: 1}},
		late: func(env *agent.ScriptEnv) ([]Binding, error) {
			sawOrdinary = env.Bindings["first"] == 1
			return []Binding{{Name: "last", Value: 2}}, nil
		},
	}

	env, err := Assemble(testInvocation(), map[string]any{"k": "v"}, provider)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !sawOrdinary {
		t.Fatal("late phase did not observe the ordinary bindings")
	}
	if env.Bindings["first"] != 1 || env.Bindings["last"] != 2 {
		t.Fatalf("bindings = %v", env.Bindings)
	}
	if env.Config["k"] != "v" {
		t.Fatalf("config = %v", env.Config)
	}
}

func TestAssembleRejectsDuplicateNames(t *testing.T) {
	cases := map[string]Provider{
		"within ordinary": ProviderFunc(func(Invocation) ([]Binding, error) {
			return []Binding{{Name: "dup", Value: 1}, {Name: "dup", Value: 2}}, nil
		}),
		"across phases": providerWithLate{
			ordinary: []Binding{{Name: "dup", Value: 1}},
			late: func(*agent.ScriptEnv) ([]Binding, error) {
				return []Binding{{Name: "dup", Value: 2}}, nil
			},
		},
		"empty name": ProviderFunc(func(Invocation) ([]Binding, error) {
			return []Binding{{Value: 1}}, nil
		}),
	}
	for name, provider := range cases {
		if _, err := Assemble(testInvocation(), nil, provider); !errdefs.IsValidation(err) {
			t.Fatalf("%s: Assemble = %v, want Validation", name, err)
		}
	}
}

func TestAssembleRejectsUnusableGlobalNames(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"leading digit": "1st",
		"dotted":        "tokens.estimate",
		"space":         "has space",
		"dash":          "has-dash",
		"dollar":        "$helper",
		"js keyword":    "var",
		"js literal":    "true",
		"lua keyword":   "end",
	}
	for name, global := range cases {
		provider := ProviderFunc(func(Invocation) ([]Binding, error) {
			return []Binding{{Name: global, Value: 1}}, nil
		})
		if _, err := Assemble(testInvocation(), nil, provider); !errdefs.IsValidation(err) {
			t.Fatalf("%s (%q): Assemble = %v, want Validation", name, global, err)
		}
	}

	for _, global := range []string{"board", "_private", "tokens2", "Host_ext"} {
		if !validGlobalName(global) {
			t.Fatalf("validGlobalName(%q) = false, want true", global)
		}
	}
}

func TestAssembleRecordsSurfaceOnTheSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("test").Start(context.Background(), "node")
	inv := Invocation{Context: ctx, NodeID: "n", NodeType: "script"}
	if _, err := Assemble(inv, nil, NewBuilder().
		Add(func(context.Context) (string, any) { return "board", 1 }).
		Add(func(context.Context) (string, any) { return "tokens", 2 })); err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	span.End()

	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(ended))
	}
	count := -1
	for _, kv := range ended[0].Attributes() {
		if kv.Key == "script.bindings.count" {
			count = int(kv.Value.AsInt64())
		}
	}
	if count != 2 {
		t.Fatalf("script.bindings.count = %d, want 2", count)
	}
}

func TestAssemblePropagatesProviderErrors(t *testing.T) {
	boom := errors.New("boom")
	provider := ProviderFunc(func(Invocation) ([]Binding, error) { return nil, boom })
	if _, err := Assemble(testInvocation(), nil, provider); !errors.Is(err, boom) {
		t.Fatalf("Assemble = %v, want boom", err)
	}

	lateErr := providerWithLate{late: func(*agent.ScriptEnv) ([]Binding, error) { return nil, boom }}
	if _, err := Assemble(testInvocation(), nil, lateErr); !errors.Is(err, boom) {
		t.Fatalf("Assemble late = %v, want boom", err)
	}
}

func TestAssembleNilProviderYieldsEmptyEnv(t *testing.T) {
	env, err := Assemble(testInvocation(), map[string]any{"k": 1}, nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(env.Bindings) != 0 {
		t.Fatalf("bindings = %v, want empty", env.Bindings)
	}
	if env.Config["k"] != 1 {
		t.Fatalf("config = %v", env.Config)
	}
}

func TestAssembleRejectsTypedNilProvider(t *testing.T) {
	var provider *typedNilProvider
	if _, err := Assemble(testInvocation(), nil, provider); !errdefs.IsValidation(err) {
		t.Fatalf("Assemble = %v, want Validation for a typed-nil provider", err)
	}
}

// typedNilProvider implements Provider on a pointer receiver so a nil
// value can be wrapped in the interface.
type typedNilProvider struct{}

func (*typedNilProvider) Bind(Invocation) ([]Binding, error) { return nil, nil }

func TestChainRunsProvidersInOrder(t *testing.T) {
	first := ProviderFunc(func(Invocation) ([]Binding, error) {
		return []Binding{{Name: "a", Value: 1}}, nil
	})
	second := providerWithLate{
		ordinary: []Binding{{Name: "b", Value: 2}},
		late: func(env *agent.ScriptEnv) ([]Binding, error) {
			if env.Bindings["a"] != 1 || env.Bindings["b"] != 2 {
				return nil, errors.New("late did not see ordinary bindings")
			}
			return []Binding{{Name: "c", Value: 3}}, nil
		},
	}

	env, err := Assemble(testInvocation(), nil, Chain(first, second))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if env.Bindings["a"] != 1 || env.Bindings["b"] != 2 || env.Bindings["c"] != 3 {
		t.Fatalf("bindings = %v", env.Bindings)
	}
}

// providerWithLate is a test provider with an optional late phase.
type providerWithLate struct {
	ordinary []Binding
	late     func(env *agent.ScriptEnv) ([]Binding, error)
}

func (p providerWithLate) Bind(Invocation) ([]Binding, error) { return p.ordinary, nil }

func (p providerWithLate) BindLate(_ Invocation, env *agent.ScriptEnv) ([]Binding, error) {
	if p.late == nil {
		return nil, nil
	}
	return p.late(env)
}
