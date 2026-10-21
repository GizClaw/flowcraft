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

// buildInlineType builds the fixed-source script-backed type.
func buildInlineType(t *testing.T, settings string, deps map[string]any) coregraph.NodeTypeRegistrar {
	t.Helper()
	value, err := (graphresource.ScriptInlineNodeTypeFactory{}).New(context.Background(), resource.NewInput([]byte(settings), deps, nil, nil))
	if err != nil {
		t.Fatalf("ScriptInlineNodeTypeFactory.New: %v", err)
	}
	return value
}

// buildScriptType builds the generic "script" type: the one whose source comes
// from each node's own config.
func buildScriptType(t *testing.T, settings string, deps map[string]any) coregraph.NodeTypeRegistrar {
	t.Helper()
	value, err := (graphresource.ScriptNodeTypeFactory{}).New(context.Background(), resource.NewInput([]byte(settings), deps, nil, nil))
	if err != nil {
		t.Fatalf("ScriptNodeTypeFactory.New: %v", err)
	}
	return value
}

// buildInferenceType builds the standard "inference" type over deps, so a unit
// test can mount it the way a deployment does: the type resource and the engine
// consume one dep map.
func buildInferenceType(t *testing.T, deps map[string]any) coregraph.NodeTypeRegistrar {
	t.Helper()
	value, err := (graphresource.InferenceNodeTypeFactory{}).New(
		context.Background(), resource.NewInput(nil, deps, nil, nil))
	if err != nil {
		t.Fatalf("InferenceNodeTypeFactory.New: %v", err)
	}
	return value
}

// buildToolType builds the standard "tool" type over deps.
func buildToolType(t *testing.T, deps map[string]any) coregraph.NodeTypeRegistrar {
	t.Helper()
	value, err := (graphresource.ToolNodeTypeFactory{}).New(
		context.Background(), resource.NewInput(nil, deps, nil, nil))
	if err != nil {
		t.Fatalf("ToolNodeTypeFactory.New: %v", err)
	}
	return value
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
	value, err := (graphresource.Factory{}).New(context.Background(), resource.NewInput([]byte(`{"graph": `+string(def)+`}`), deps, nil, nil))
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	engine := value
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
	registrar := buildInlineType(t, `{
		"type": "greet",
		"source": "fake source",
		"desc": "greets a name",
		"writes": [{"kind": "var", "name": "greeting", "required": true}]
	}`, map[string]any{"script_runtime": rt})

	// The node config payload reaches the script as its config global,
	// with ${board:*} references resolved first.
	board := agent.NewBoard()
	board.SetVar("who", "world")
	engine := buildCustomEngine(t,
		customNodeDefinition(t, "greet", map[string]any{"name": "${board:who}"}),
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
	if v, _ := board.Var("greeting"); v != "hi world" {
		t.Fatalf("greeting = %v, want the script's write", v)
	}
}

func TestEngineMountsCustomNodeTypesViaManyDeps(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(_ context.Context, _ string, _ string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		board := env.Bindings["board"].(map[string]any)
		board["setVar"].(func(string, any))("seen", env.Config["value"])
		return nil, nil
	}}
	registrar := buildInlineType(t, `{
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
	if v, _ := board.Var("seen"); v != float64(7) {
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
	registrar := buildInlineType(t, `{
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
	if v, _ := board.Var("out"); v != "processed:payload" {
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
	_, err := (graphresource.Factory{}).New(context.Background(), resource.NewInput([]byte(`{"graph": `+string(def)+`}`), map[string]any{"node_type": "not a registrar"}, nil, nil))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("New = %v, want Validation for non-registrar node_type dep", err)
	}
}

type nilRegistrar struct{}

func (*nilRegistrar) Register(*coregraph.Registry) error { return nil }

func TestEngineRejectsTypedNilNodeTypeDep(t *testing.T) {
	def := customNodeDefinition(t, "whatever", map[string]any{})
	var registrar *nilRegistrar // typed nil
	_, err := (graphresource.Factory{}).New(context.Background(), resource.NewInput([]byte(`{"graph": `+string(def)+`}`), map[string]any{"node_type": registrar}, nil, nil))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("New = %v, want Validation for typed-nil node_type dep", err)
	}
}

func TestEngineRejectsDuplicateMount(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	registrar := buildInlineType(t, `{"type": "dup", "source": "s"}`, map[string]any{"script_runtime": rt})

	def := customNodeDefinition(t, "dup", map[string]any{})
	_, err := (graphresource.Factory{}).New(context.Background(), resource.NewInput([]byte(`{"graph": `+string(def)+`}`), map[string]any{
		"node_type.a": registrar,
		"node_type.b": registrar,
	}, nil, nil))
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
	first := buildInlineType(t, `{"type": "dup", "source": "s1"}`, map[string]any{"script_runtime": rt})
	second := buildInlineType(t, `{"type": "dup", "source": "s2"}`, map[string]any{"script_runtime": rt})

	def := customNodeDefinition(t, "dup", map[string]any{})
	_, err := (graphresource.Factory{}).New(context.Background(), resource.NewInput([]byte(`{"graph": `+string(def)+`}`), map[string]any{
		"node_type.a": first,
		"node_type.b": second,
	}, nil, nil))
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("New = %v, want Conflict for duplicate custom type", err)
	}
	if !strings.Contains(err.Error(), "node_type.a") || !strings.Contains(err.Error(), "node_type.b") {
		t.Fatalf("New error = %v, want both dep keys named", err)
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
	value, err := (graphresource.Factory{}).New(context.Background(), resource.NewInput([]byte(`{"graph": `+string(def)+`}`), map[string]any{"node_type.go_echo": registrar}, loader, nil))
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}

	board := agent.NewBoard()
	if err := runCustomEngine(t, value, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Prompt != "hello from file" {
		t.Fatalf("handler received prompt %q, want materialized file content", got.Prompt)
	}
	if v, _ := board.Var("out"); v != "hello from file" {
		t.Fatalf("out = %v, want file content", v)
	}
}

func TestScriptInlineNodeTypeFactoryValidation(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	cases := []struct {
		name     string
		settings string
	}{
		{"missing source", `{"type": "x"}`},
		{"unknown field", `{"type": "x", "source": "s", "bogus": 1}`},
		{"whitespace type", `{"type": "two words", "source": "s"}`},
		{"unknown runtime", `{"type": "x", "source": "s", "runtime": "lua"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (graphresource.ScriptInlineNodeTypeFactory{}).New(context.Background(), resource.NewInput([]byte(tc.settings), map[string]any{"script_runtime": rt}, nil, nil))
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("New = %v, want Validation", err)
			}
		})
	}
}

// TestScriptInlineTypeDefaultsToItsImplName pins the default type name: a
// deployment that mounts one inline type names nothing and graphs use
// "script_inline".
func TestScriptInlineTypeDefaultsToItsImplName(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	registrar := buildInlineType(t, `{"source": "s"}`, map[string]any{"script_runtime": rt})
	provider, ok := registrar.(coregraph.TypeNameProvider)
	if !ok {
		t.Fatal("inline type does not report its names")
	}
	if names := provider.NodeTypeNames(); len(names) != 1 || names[0] != graphresource.NodeTypeImplScriptInline {
		t.Fatalf("NodeTypeNames = %v, want [%s]", names, graphresource.NodeTypeImplScriptInline)
	}
}

// TestScriptTypeRunsNodeOwnedSource pins the generic type: the source belongs to
// the node, and the node names its runtime.
func TestScriptTypeRunsNodeOwnedSource(t *testing.T) {
	var gotSource string
	rt := scriptRuntimeStub{exec: func(_ context.Context, _ string, source string, env *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		gotSource = source
		board := env.Bindings["board"].(map[string]any)
		board["setVar"].(func(string, any))("seen", env.Config["value"])
		return nil, nil
	}}
	caps := map[string]any{"script_runtime.js": rt}
	engine := buildCustomEngine(t,
		customNodeDefinition(t, "script", map[string]any{"runtime": "js", "source": "node source", "config": map[string]any{"value": 3}}),
		map[string]any{"node_type.script": buildScriptType(t, `{}`, caps)})

	board := agent.NewBoard()
	if err := runCustomEngine(t, engine, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotSource != "node source" {
		t.Fatalf("source = %q, want the node's own source", gotSource)
	}
	if v, _ := board.Var("seen"); v != float64(3) {
		t.Fatalf("seen = %v, want the node config global", v)
	}
}

// TestScriptTypeRequiresNamedRuntimes pins the one dep shape the generic type
// cannot work without: an unsuffixed "script_runtime" leaves no name for a
// node's config.runtime to select, so it is refused at build time with the
// spelling that would work.
func TestScriptTypeRequiresNamedRuntimes(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	_, err := (graphresource.ScriptNodeTypeFactory{}).New(context.Background(),
		resource.NewInput([]byte(`{}`), map[string]any{"script_runtime": rt}, nil, nil))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("New = %v, want Validation", err)
	}
	if !strings.Contains(err.Error(), "script_runtime.<name>") {
		t.Fatalf("New error = %v, want the suffixed spelling", err)
	}
}

// TestScriptTypeRejectsUnwiredRuntimeAtBuild pins the check that moved from the
// engine into the type: a node naming a runtime the type was not wired with
// fails the deployment, not the first turn.
func TestScriptTypeRejectsUnwiredRuntimeAtBuild(t *testing.T) {
	rt := scriptRuntimeStub{exec: func(context.Context, string, string, *agent.ScriptEnv) (*agent.ScriptSignal, error) {
		return nil, nil
	}}
	registrar := buildScriptType(t, `{}`, map[string]any{"script_runtime.js": rt})
	_, err := (graphresource.Factory{}).New(context.Background(), resource.NewInput(
		[]byte(`{"graph": `+string(customNodeDefinition(t, "script", map[string]any{"runtime": "lua", "source": "x"}))+`}`),
		map[string]any{"node_type.script": registrar}, nil, nil))
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("engine New = %v, want NotFound for an unwired runtime", err)
	}
	if !strings.Contains(err.Error(), `"lua"`) || !strings.Contains(err.Error(), "js") {
		t.Fatalf("engine New error = %v, want the missing name and the mounted ones", err)
	}
}

// TestToolTypeRequiresToolsDep pins the one dep the type cannot work without:
// a tool node with no catalog has nothing to execute, so the deployment is
// refused with the dep key that would fix it.
func TestToolTypeRequiresToolsDep(t *testing.T) {
	_, err := (graphresource.ToolNodeTypeFactory{}).New(context.Background(),
		resource.NewInput(nil, nil, nil, nil))
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("New = %v, want NotFound for the missing tools dep", err)
	}
	if !strings.Contains(err.Error(), graphresource.DepTools) {
		t.Fatalf("New error = %v, want the dep key named", err)
	}
}

// TestToolTypeRunsTheChannelBatch pins the built-in "tool" type end to end
// through the resource layer: a graph naming "tool" is served the dispatcher
// the type was wired with, and the model's call id survives the round trip.
func TestToolTypeRunsTheChannelBatch(t *testing.T) {
	engine := buildCustomEngine(t,
		customNodeDefinition(t, "tool", map[string]any{"results_key": "results"}),
		map[string]any{"node_type.tool": buildToolType(t, map[string]any{"tools": newToolAssembly(t)})})

	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.Message{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.ToolCallPart{Call: message.ToolCall{
				ID:        "call_1",
				Name:      "echo",
				Arguments: json.RawMessage(`{"q":"hi"}`),
			}},
		}},
	})
	// Executed directly rather than through runCustomEngine, which appends a
	// user turn and would put it on the tail this node reads.
	if _, err := engine.Execute(context.Background(),
		agent.Run{Identity: agent.Identity{AgentID: "test-agent", RunID: "run-1"}},
		agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 || msgs[1].Role != message.RoleTool {
		t.Fatalf("channel = %+v, want the assistant message plus one role=tool message", msgs)
	}
	results, ok := board.Var("results")
	if !ok {
		t.Fatal("results_key did not receive the batch")
	}
	batch, ok := results.([]message.ToolResult)
	if !ok || len(batch) != 1 || batch[0].CallID != "call_1" {
		t.Fatalf("results = %#v, want one result keeping call_1", results)
	}
}
