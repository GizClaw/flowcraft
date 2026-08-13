package script

import (
	"context"
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// fakeRuntime captures what the node hands the script runtime. The
// script contract (agent.ScriptRuntime) has no canned test
// implementation — the real runtimes live in integrations/agent — so the node
// tests stub the one-method interface inline.
type fakeRuntime struct {
	exec func(ctx context.Context, name, source string, env *agent.ScriptEnv) (*agent.ScriptSignal, error)
}

func (f fakeRuntime) Exec(ctx context.Context, name, source string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
	return f.exec(ctx, name, source, env)
}

func scriptRegistry(t *testing.T, deps ScriptNodeDeps) *graph.Registry {
	t.Helper()
	reg := graph.NewRegistry()
	if err := graph.RegisterType(reg, "script", NewNode(deps)); err != nil {
		t.Fatalf("register script: %v", err)
	}
	return reg
}

func singleScriptGraph(t *testing.T, reg *graph.Registry, config any) *graph.Graph {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	g, err := graph.Build(&graph.GraphDefinition{
		Name:  "test-graph",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{ID: "n", Type: "script", Config: raw}},
	}, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

func executeGraph(g *graph.Graph, board *agent.Board) error {
	return executeGraphWithHost(g, agent.NoopHost{}, board)
}

func executeGraphWithHost(g *graph.Graph, host agent.Host, board *agent.Board) error {
	_, err := g.Execute(context.Background(),
		agent.Run{Identity: agent.Identity{AgentID: "test-agent", RunID: "run-1"}},
		host, board)
	return err
}

func TestScriptNode_ExecReceivesEnvAndConfig(t *testing.T) {
	var gotName, gotSource string
	var gotEnv *agent.ScriptEnv
	rt := fakeRuntime{exec: func(_ context.Context, name, source string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		gotName, gotSource, gotEnv = name, source, env
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{
		Runtime: "fake",
		Source:  "print(1)",
		Config:  map[string]any{"threshold": 0.5},
	})
	if err := executeGraph(g, agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if gotName != "n" {
		t.Fatalf("name = %q, want the node id as default", gotName)
	}
	if gotSource != "print(1)" {
		t.Fatalf("source = %q", gotSource)
	}
	if gotEnv.Config["threshold"] != 0.5 {
		t.Fatalf("env.Config = %v, want threshold passthrough", gotEnv.Config)
	}
	// The standard bridge set, including the late runtime bridge.
	for _, want := range []string{"board", "expr", "host", "run", "tools", "inference", "runtime"} {
		if _, ok := gotEnv.Bindings[want]; !ok {
			t.Fatalf("env.Bindings missing %q (has %v)", want, keys(gotEnv.Bindings))
		}
	}
}

func TestScriptNode_ConfigMayInterpolateBoardRefs(t *testing.T) {
	var gotEnv *agent.ScriptEnv
	rt := fakeRuntime{exec: func(_ context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		gotEnv = env
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, map[string]any{
		"runtime": "fake",
		"source":  "x",
		"config":  map[string]any{"query": "${board.user_query}"},
	})
	board := agent.NewBoard()
	board.SetVar("user_query", "weather in paris")
	if err := executeGraph(g, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := gotEnv.Config["query"]; got != "weather in paris" {
		t.Fatalf("config query = %v, want board reference resolved", got)
	}
}

func TestScriptNode_RuntimeNotWired(t *testing.T) {
	reg := scriptRegistry(t, ScriptNodeDeps{})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "js", Source: "x"})
	if err := executeGraph(g, agent.NewBoard()); err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("unwired runtime error = %v, want NotAvailable", err)
	}
}

func TestScriptNode_SignalMapsToError(t *testing.T) {
	rt := fakeRuntime{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return &agent.ScriptSignal{Type: "error", Kind: "validation", Message: "bad input"}, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	if err := executeGraph(g, agent.NewBoard()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("error signal = %v, want validation-classified", err)
	}
}

func TestScriptNode_ScriptMutatesTheSameBoard(t *testing.T) {
	rt := fakeRuntime{exec: func(_ context.Context, _, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		board := env.Bindings["board"].(map[string]any)
		board["setVar"].(func(string, any))("done", true)
		return nil, nil
	}}
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": rt}})
	g := singleScriptGraph(t, reg, ScriptConfig{Runtime: "fake", Source: "x"})
	board := agent.NewBoard()
	if err := executeGraph(g, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if v, ok := board.GetVar("done"); !ok || v != true {
		t.Fatalf("board var done = %v, want true — script writes must land on the graph board", v)
	}
}

func TestDecodeScriptConfig(t *testing.T) {
	if _, err := decodeScriptConfig(json.RawMessage(`{"source":"x"}`)); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing runtime error = %v, want validation-classified", err)
	}
	if _, err := decodeScriptConfig(json.RawMessage(`{"runtime":"js"}`)); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing source error = %v, want validation-classified", err)
	}
	// Strict decoding: unknown top-level keys are typos, not script
	// config — script config lives under "config".
	if _, err := decodeScriptConfig(json.RawMessage(`{"runtime":"js","source":"x","soruce":"typo"}`)); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("unknown field error = %v, want validation-classified", err)
	}
	cfg, err := decodeScriptConfig(json.RawMessage(`{"runtime":"js","name":"job","source":"x","config":{"k":1}}`))
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if cfg.Runtime != "js" || cfg.Name != "job" || cfg.Source != "x" || cfg.Config["k"] != float64(1) {
		t.Fatalf("decoded = %+v", cfg)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRegister(t *testing.T) {
	reg := graph.NewRegistry()
	if err := Register(reg, ScriptNodeDeps{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !reg.Has("script") {
		t.Fatal("type \"script\" not registered")
	}
	if err := Register(reg, ScriptNodeDeps{}); err == nil {
		t.Fatal("duplicate Register succeeded")
	}
}

// TestScriptNode_FSShellBridgesOptIn proves the fs/shell globals are
// bound only when their host capability is wired: no workspace → no
// "fs", no runner → no "shell".
func TestScriptNode_FSShellBridgesOptIn(t *testing.T) {
	capture := func(env **agent.ScriptEnv) fakeRuntime {
		return fakeRuntime{exec: func(_ context.Context, _, _ string, e *agent.ScriptEnv) (*agent.ScriptSignal, error) {
			*env = e
			return nil, nil
		}}
	}
	cfg := ScriptConfig{Runtime: "fake", Source: "print(1)"}

	// Unwired: neither global is bound.
	var bare *agent.ScriptEnv
	reg := scriptRegistry(t, ScriptNodeDeps{Runtimes: map[string]agent.ScriptRuntime{"fake": capture(&bare)}})
	if err := executeGraph(singleScriptGraph(t, reg, cfg), agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, unwanted := range []string{"fs", "shell"} {
		if _, ok := bare.Bindings[unwanted]; ok {
			t.Fatalf("%q bound without its capability (has %v)", unwanted, keys(bare.Bindings))
		}
	}

	// Wired: both globals appear.
	var wired *agent.ScriptEnv
	reg = scriptRegistry(t, ScriptNodeDeps{
		Runtimes:      map[string]agent.ScriptRuntime{"fake": capture(&wired)},
		Workspace:     stubWorkspace{},
		CommandRunner: stubRunner{},
	})
	if err := executeGraph(singleScriptGraph(t, reg, cfg), agent.NewBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"fs", "shell"} {
		if _, ok := wired.Bindings[want]; !ok {
			t.Fatalf("env.Bindings missing %q (has %v)", want, keys(wired.Bindings))
		}
	}
}

type stubWorkspace struct{}

func (stubWorkspace) Read(context.Context, string) ([]byte, error) { return nil, nil }
func (stubWorkspace) Write(context.Context, string, []byte) error  { return nil }
func (stubWorkspace) Append(context.Context, string, []byte) error { return nil }
func (stubWorkspace) Rename(context.Context, string, string) error { return nil }
func (stubWorkspace) Delete(context.Context, string) error         { return nil }
func (stubWorkspace) RemoveAll(context.Context, string) error      { return nil }
func (stubWorkspace) List(context.Context, string) ([]fs.DirEntry, error) {
	return nil, nil
}
func (stubWorkspace) Exists(context.Context, string) (bool, error)      { return false, nil }
func (stubWorkspace) Stat(context.Context, string) (fs.FileInfo, error) { return nil, nil }

type stubRunner struct{}

func (stubRunner) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }
func (stubRunner) Start(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, errdefs.NotAvailablef("script node test: stub runner cannot start sessions")
}
func (stubRunner) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }
func (stubRunner) Terminate(context.Context, string) error             { return nil }
