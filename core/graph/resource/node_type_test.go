package resource_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	graphresource "github.com/GizClaw/flowcraft/core/graph/resource"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

// scriptRuntimeStub stubs the one-method agent.ScriptRuntime contract.
type scriptRuntimeStub struct {
	exec func(ctx context.Context, name, source string, env *agent.ScriptEnv) (*agent.ScriptSignal, error)
}

func (s scriptRuntimeStub) Exec(ctx context.Context, name, source string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
	return s.exec(ctx, name, source, env)
}

func buildScriptNodeType(t *testing.T, settings string, deps map[string]any) coregraph.NodeTypeRegistrar {
	t.Helper()
	value, err := (graphresource.ScriptNodeTypeFactory{}).New(context.Background(), resource.Input{
		Settings: []byte(settings),
		Deps:     deps,
	})
	if err != nil {
		t.Fatalf("ScriptNodeTypeFactory.New: %v", err)
	}
	registrar, ok := value.(coregraph.NodeTypeRegistrar)
	if !ok {
		t.Fatalf("ScriptNodeTypeFactory.New returned %T, want graph.NodeTypeRegistrar", value)
	}
	return registrar
}

func customNodeDefinition(t *testing.T, nodeType string, config any) []byte {
	t.Helper()
	def, err := json.Marshal(map[string]any{
		"name":  "g",
		"entry": "n1",
		"nodes": []any{
			map[string]any{
				"id":     "n1",
				"type":   nodeType,
				"config": config,
			},
		},
		"edges": []any{},
	})
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	return def
}

func buildCustomEngine(t *testing.T, def []byte, deps map[string]any) *coregraph.Graph {
	t.Helper()
	value, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
		Deps:     deps,
	})
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	engine, ok := value.(*coregraph.Graph)
	if !ok {
		t.Fatalf("engine New returned %T, want *coregraph.Graph", value)
	}
	return engine
}

func runCustomEngine(t *testing.T, g *coregraph.Graph, board *agent.Board) error {
	t.Helper()
	if board == nil {
		board = agent.NewBoard()
	}
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleUser, "hi"))
	_, err := g.Execute(context.Background(),
		agent.Run{Identity: agent.Identity{AgentID: "test-agent", RunID: "run-1"}},
		agent.NoopHost{}, board)
	return err
}

func TestEngineMountsCustomScriptNode(t *testing.T) {
	var gotName, gotSource string
	var gotEnv *agent.ScriptEnv
	rt := scriptRuntimeStub{exec: func(_ context.Context, name, source string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		gotName, gotSource, gotEnv = name, source, env
		board := env.Bindings["board"].(map[string]any)
		board["setVar"].(func(string, any))("greeting", "hi "+env.Config["name"].(string))
		return nil, nil
	}}
	registrar := buildScriptNodeType(t, `{
		"type": "greet",
		"source": "fake source",
		"desc": "greets a name",
		"writes": [{"kind": "var", "name": "greeting", "required": true}]
	}`, map[string]any{"script_runtime": rt})

	// The node config payload reaches the script as its config global,
	// with ${board.*} references resolved first.
	board := agent.NewBoard()
	board.SetVar("who", "world")
	engine := buildCustomEngine(t,
		customNodeDefinition(t, "greet", map[string]any{"name": "${board.who}"}),
		map[string]any{"node_type": registrar})

	if err := runCustomEngine(t, engine, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotName != "n1" {
		t.Fatalf("name = %q, want the node id", gotName)
	}
	if gotSource != "fake source" {
		t.Fatalf("source = %q", gotSource)
	}
	if gotEnv.Config["name"] != "world" {
		t.Fatalf("env.Config = %v, want board-resolved name", gotEnv.Config)
	}
	if v, _ := board.GetVar("greeting"); v != "hi world" {
		t.Fatalf("greeting = %v, want the script's write", v)
	}
}

func TestEngineMountsCustomNodeTypesViaManyDeps(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(_ context.Context, _ string, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		board := env.Bindings["board"].(map[string]any)
		board["setVar"].(func(string, any))("seen", env.Config["value"])
		return nil, nil
	}}
	registrar := buildScriptNodeType(t, `{
		"type": "passthrough",
		"source": "fake source",
		"writes": [{"kind": "var", "name": "seen", "required": true}]
	}`, map[string]any{"script_runtime": rt})

	engine := buildCustomEngine(t,
		customNodeDefinition(t, "passthrough", map[string]any{"value": 7}),
		map[string]any{"node_type.passthrough": registrar})

	board := agent.NewBoard()
	if err := runCustomEngine(t, engine, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if v, _ := board.GetVar("seen"); v != float64(7) {
		t.Fatalf("seen = %v, want 7", v)
	}
}

func TestCustomNodeConfigKeyRoles(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(_ context.Context, _ string, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		board := env.Bindings["board"].(map[string]any)
		input := board["getVar"].(func(string) any)("doc")
		board["setVar"].(func(string, any))("out", "processed:"+input.(string))
		return nil, nil
	}}
	registrar := buildScriptNodeType(t, `{
		"type": "transform",
		"source": "fake source",
		"reads":  [{"kind": "var", "config_key": "input_key", "required": true}],
		"writes": [{"kind": "var", "config_key": "result_key", "required": true}]
	}`, map[string]any{"script_runtime": rt})

	engine := buildCustomEngine(t,
		customNodeDefinition(t, "transform", map[string]any{"input_key": "doc", "result_key": "out"}),
		map[string]any{"node_type.transform": registrar})

	board := agent.NewBoard()
	board.SetVar("doc", "payload")
	if err := runCustomEngine(t, engine, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if v, _ := board.GetVar("out"); v != "processed:payload" {
		t.Fatalf("out = %v, want processed payload", v)
	}

	// Required read roles are enforced at invocation: a board without
	// the bound var fails the run.
	missing := agent.NewBoard()
	if err := runCustomEngine(t, engine, missing); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Execute without required read = %v, want Validation", err)
	}
}

func TestEngineRejectsNonRegistrarNodeTypeDep(t *testing.T) {
	def := customNodeDefinition(t, "whatever", map[string]any{})
	_, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
		Deps:     map[string]any{"node_type": "not a registrar"},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("New = %v, want Validation for non-registrar node_type dep", err)
	}
}

type nilRegistrar struct{}

func (*nilRegistrar) Register(*coregraph.Registry) error { return nil }

func TestEngineRejectsTypedNilNodeTypeDep(t *testing.T) {
	def := customNodeDefinition(t, "whatever", map[string]any{})
	var registrar *nilRegistrar // typed nil
	_, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
		Deps:     map[string]any{"node_type": registrar},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("New = %v, want Validation for typed-nil node_type dep", err)
	}
}

func TestEngineRejectsDuplicateMount(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	registrar := buildScriptNodeType(t, `{"type": "dup", "source": "s"}`, map[string]any{"script_runtime": rt})

	def := customNodeDefinition(t, "dup", map[string]any{})
	_, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
		Deps: map[string]any{
			"node_type.a": registrar,
			"node_type.b": registrar,
		},
	})
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("New = %v, want Conflict for duplicate mount", err)
	}
	if !strings.Contains(err.Error(), "node_type.a") || !strings.Contains(err.Error(), "node_type.b") {
		t.Fatalf("New error = %v, want both dep keys named", err)
	}
}

func TestEngineRejectsDuplicateCustomNodeType(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	first := buildScriptNodeType(t, `{"type": "dup", "source": "s1"}`, map[string]any{"script_runtime": rt})
	second := buildScriptNodeType(t, `{"type": "dup", "source": "s2"}`, map[string]any{"script_runtime": rt})

	def := customNodeDefinition(t, "dup", map[string]any{})
	_, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
		Deps: map[string]any{
			"node_type.a": first,
			"node_type.b": second,
		},
	})
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("New = %v, want Conflict for duplicate custom type", err)
	}
}

type goEchoConfig struct {
	Prompt    string `json:"prompt"`
	OutputKey string `json:"output_key"`
}

// goEchoRegistrar is a host-defined Go node type: it registers a
// typed node via graph.RegisterType and opts into config file-ref
// materialization via ConfigFileRefFields.
type goEchoRegistrar struct {
	got *goEchoConfig
}

func (r goEchoRegistrar) Register(reg *coregraph.Registry) error {
	return coregraph.RegisterType(reg, "go_echo", coregraph.NodeType[goEchoConfig]{
		Meta: coregraph.Meta{
			Writes: []coregraph.Role{{
				Kind: coregraph.RoleVar, ConfigKey: "output_key", Required: true,
			}},
		},
		Handler: func(_ coregraph.ExecutionContext, board *agent.Board, cfg goEchoConfig) error {
			*r.got = cfg
			board.SetVar(cfg.OutputKey, cfg.Prompt)
			return nil
		},
	})
}

func (r goEchoRegistrar) FileRefFields() map[string][]string {
	return map[string][]string{"go_echo": {"prompt"}}
}

func TestEngineMountsGoBackedNodeTypeWithFileRefs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("hello from file"), 0o600); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}
	loader := resource.NewLoader(resource.WithBaseDir(dir))

	var got goEchoConfig
	registrar := goEchoRegistrar{got: &got}
	def := customNodeDefinition(t, "go_echo", map[string]any{
		"prompt":     map[string]any{"file": "prompt.txt"},
		"output_key": "out",
	})
	value, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
		Deps:     map[string]any{"node_type.go_echo": registrar},
		Loader:   loader,
	})
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}

	board := agent.NewBoard()
	if err := runCustomEngine(t, value.(*coregraph.Graph), board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Prompt != "hello from file" {
		t.Fatalf("handler received prompt %q, want materialized file content", got.Prompt)
	}
	if v, _ := board.GetVar("out"); v != "hello from file" {
		t.Fatalf("out = %v, want file content", v)
	}
}

func TestScriptNodeTypeFactoryValidation(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	cases := []struct {
		name     string
		settings string
	}{
		{"missing type", `{"source": "s"}`},
		{"missing source", `{"type": "x"}`},
		{"unknown field", `{"type": "x", "source": "s", "bogus": 1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (graphresource.ScriptNodeTypeFactory{}).New(context.Background(), resource.Input{
				Settings: []byte(tc.settings),
				Deps:     map[string]any{"script_runtime": rt},
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("New = %v, want Validation", err)
			}
		})
	}
}
