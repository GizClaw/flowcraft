package script

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/graph"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// bindingNames returns the sorted global names of an assembled env.
func bindingNames(env *agent.ScriptEnv) []string {
	out := make([]string, 0, len(env.Bindings))
	for name := range env.Bindings {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestStandardBindingsExposeTheStandardSurface(t *testing.T) {
	rt := fakeRuntime{}
	inv := bindings.Invocation{
		Context:  context.Background(),
		Board:    agent.NewBoard(),
		Host:     agent.NoopHost{},
		Name:     "n",
		NodeID:   "n",
		NodeType: "script",
		GraphID:  "g",
		RunInfo:  agent.RunInfo{Identity: agent.Identity{AgentID: "a", RunID: "run-1"}},
		Runtime:  rt,
	}
	env, err := bindings.Assemble(inv, nil, StandardBindings(ScriptNodeDeps{}))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	got := bindingNames(env)
	want := []string{
		"board", "expr", "host", "inference", "node",
		"parallel", "run", "runtime", "stream", "tools",
	}
	if len(got) != len(want) {
		t.Fatalf("globals = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("globals = %v, want %v", got, want)
		}
	}
	// fs and shell stay opt-in: neither the workspace nor the sandbox
	// dep is wired.
	if _, ok := env.Bindings["fs"]; ok {
		t.Fatal("fs must stay unwired without a workspace dep")
	}
	if _, ok := env.Bindings["shell"]; ok {
		t.Fatal("shell must stay unwired without a sandbox dep")
	}
}

func TestStandardBindingsRequireARuntime(t *testing.T) {
	inv := bindings.Invocation{
		Context: context.Background(),
		Board:   agent.NewBoard(),
		Host:    agent.NoopHost{},
	}
	_, err := bindings.Assemble(inv, nil, StandardBindings(ScriptNodeDeps{}))
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Assemble = %v, want NotAvailable without an executing runtime", err)
	}
}

func TestScriptNode_EngineBindingsReplaceStandardSurface(t *testing.T) {
	rt := fakeRuntime{}
	var gotEnv *agent.ScriptEnv
	rt.exec = func(_ context.Context, _ string, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		gotEnv = env
		return nil, nil
	}

	var gotInv bindings.Invocation
	provider := bindings.ProviderFunc(func(inv bindings.Invocation) ([]bindings.Binding, error) {
		gotInv = inv
		return []bindings.Binding{{
			Name: "tokens",
			Value: map[string]any{
				"estimate": func() int { return 42 },
			},
		}}, nil
	})

	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"js": rt}})
	g, err := graph.Build(&graph.GraphDefinition{
		Name:  "test-graph",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{
			ID:   "n",
			Type: "script",
			Config: json.RawMessage(
				`{"runtime":"js","source":"x","name":"compact"}`),
		}},
	}, reg, graph.WithScriptBindings(provider))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := executeGraph(g, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if gotEnv == nil {
		t.Fatal("script runtime did not run")
	}
	if _, ok := gotEnv.Bindings["tokens"]; !ok {
		t.Fatalf("bindings = %v, want the engine-level tokens global", gotEnv.Bindings)
	}
	// The engine-level provider replaces the standard surface rather
	// than extending it.
	if _, ok := gotEnv.Bindings["board"]; ok {
		t.Fatalf("bindings = %v, want no standard board global", gotEnv.Bindings)
	}

	// The provider receives this invocation's state.
	if gotInv.NodeID != "n" || gotInv.NodeType != "script" || gotInv.GraphID != "test-graph" {
		t.Fatalf("invocation identity = %+v", gotInv)
	}
	if gotInv.Name != "compact" {
		t.Fatalf("invocation name = %q, want the config label", gotInv.Name)
	}
	if gotInv.RunInfo.RunID != "run-1" {
		t.Fatalf("invocation run info = %+v", gotInv.RunInfo)
	}
	if gotInv.Board == nil || gotInv.Host == nil {
		t.Fatalf("invocation state = %+v", gotInv)
	}
	if _, ok := gotInv.Runtime.(fakeRuntime); !ok {
		t.Fatalf("invocation runtime = %T, want the executing runtime", gotInv.Runtime)
	}
	if gotInv.Emit == nil {
		t.Fatal("invocation emit is nil")
	}
}

func TestScriptNode_AssemblyFailureFailsTheNode(t *testing.T) {
	rt := fakeRuntime{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		t.Fatal("runtime must not run when assembly fails")
		return nil, nil
	}}
	provider := bindings.ProviderFunc(func(bindings.Invocation) ([]bindings.Binding, error) {
		return []bindings.Binding{{Name: "dup", Value: 1}, {Name: "dup", Value: 2}}, nil
	})

	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"js": rt}})
	g, err := graph.Build(&graph.GraphDefinition{
		Name:  "test-graph",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{
			ID:     "n",
			Type:   "script",
			Config: json.RawMessage(`{"runtime":"js","source":"x"}`),
		}},
	}, reg, graph.WithScriptBindings(provider))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := executeGraph(g, agent.NewBoard()); err == nil {
		t.Fatal("Execute = nil, want the duplicate-binding failure")
	}
}

func TestScriptNode_NodeSpanReportsTheSurface(t *testing.T) {
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	rt := fakeRuntime{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"js": rt}})
	config := json.RawMessage(`{"runtime":"js","source":"x"}`)

	// Engine-level provider: the span reports the engine surface.
	engineGraph, err := graph.Build(&graph.GraphDefinition{
		Name:  "engine-graph",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{ID: "n", Type: "script", Config: config}},
	}, reg, graph.WithScriptBindings(
		bindings.ProviderFunc(func(bindings.Invocation) ([]bindings.Binding, error) {
			return []bindings.Binding{{Name: "tokens", Value: 1}}, nil
		})))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := executeGraph(engineGraph, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// No provider: the node falls back to its own standard bindings.
	standardGraph := singleScriptGraph(t, reg, map[string]any{"runtime": "js", "source": "x"})
	if err := executeGraph(standardGraph, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	surfaces := map[string]string{}
	for _, span := range rec.Ended() {
		if !strings.HasPrefix(span.Name(), "node.") {
			continue
		}
		for _, kv := range span.Attributes() {
			if kv.Key == "script.bindings.source" {
				surfaces[kv.Value.AsString()] = span.Name()
			}
		}
	}
	if _, ok := surfaces["engine"]; !ok {
		t.Fatalf("node spans = %v, want an engine-source surface", surfaces)
	}
	if _, ok := surfaces["standard"]; !ok {
		t.Fatalf("node spans = %v, want a standard-source surface", surfaces)
	}
}
