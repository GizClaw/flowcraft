package scriptnode

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	nodepkg "github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func testValueBridge(value string) bindings.BindingFunc {
	return func(context.Context) (string, any) {
		return "extra", map[string]any{
			"value": func() string { return value },
		}
	}
}

type readFileErrorFS struct {
	err error
}

func (f readFileErrorFS) Open(string) (fs.File, error) {
	return nil, fs.ErrNotExist
}

func (f readFileErrorFS) ReadFile(string) ([]byte, error) {
	return nil, f.err
}

func TestRegister_ExtraBridgesAvailableToBuiltinScriptNode(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: rt,
		ExtraBridges:  []bindings.BindingFunc{testValueBridge("from-extra")},
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "iter_extra",
		Type: "iteration",
		Config: map[string]any{
			"input_key":   "items",
			"body_script": `board.setVar("__iteration_result", extra.value());`,
		},
	})
	if err != nil {
		t.Fatalf("Build iteration: %v", err)
	}

	board := graph.NewBoard()
	board.SetVar("items", []any{"one"})
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: context.Background()}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}

	raw, _ := board.GetVar("iteration_results")
	results, ok := raw.([]any)
	if !ok {
		t.Fatalf("iteration_results shape = %T", raw)
	}
	if len(results) != 1 || results[0] != "from-extra" {
		t.Fatalf("iteration_results = %v, want [from-extra]", results)
	}
}

func TestRegister_ExtraBridgesAvailableToTypeScriptNode(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: rt,
		ExtraBridges: []bindings.BindingFunc{
			testValueBridge("first"),
			testValueBridge("second"),
		},
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "script_extra",
		Type: "script",
		Config: map[string]any{
			"source": `board.setVar("extra_value", extra.value());`,
		},
	})
	if err != nil {
		t.Fatalf("Build script: %v", err)
	}

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: context.Background()}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("extra_value"); got != "second" {
		t.Fatalf("extra_value = %v, want second", got)
	}
}

func TestRegister_ExtraBridgesAvailableToScriptFSFallbackNode(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: rt,
		ScriptFS: fstest.MapFS{
			"custom.js": {Data: []byte(`board.setVar("extra_value", extra.value());`)},
		},
		ExtraBridges: []bindings.BindingFunc{testValueBridge("from-fs")},
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "fallback_extra",
		Type: "custom",
	})
	if err != nil {
		t.Fatalf("Build fallback: %v", err)
	}

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: context.Background()}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("extra_value"); got != "from-fs" {
		t.Fatalf("extra_value = %v, want from-fs", got)
	}
}

func TestRegister_ExtraBridgesCopiedForConstructedNodes(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	extras := []bindings.BindingFunc{testValueBridge("before-mutation")}
	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: rt,
		ExtraBridges:  extras,
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "script_alias",
		Type: "script",
		Config: map[string]any{
			"source": `board.setVar("extra_value", extra.value());`,
		},
	})
	if err != nil {
		t.Fatalf("Build script: %v", err)
	}
	extras[0] = testValueBridge("after-mutation")

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: context.Background()}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("extra_value"); got != "before-mutation" {
		t.Fatalf("extra_value = %v, want before-mutation", got)
	}
}

func TestRegister_ContextFilesInjectsFSBridge(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	if err := ws.Write(ctx, "docs/note.md", []byte("hello from workspace")); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		Workspace:     ws,
	})
	n, err := factory.Build(graph.NodeDefinition{
		ID:   "ctx_files",
		Type: "context",
		Config: map[string]any{
			"files": []any{
				map[string]any{"path": "docs/note.md", "state_key": "note"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build context: %v", err)
	}

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: ctx}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("note"); got != "hello from workspace" {
		t.Fatalf("note = %v, want workspace file content", got)
	}
}

func TestRegister_ScriptNodeInjectsFSBridge(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	if err := ws.Write(ctx, "docs/script.txt", []byte("from-script-workspace")); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		Workspace:     ws,
	})
	n, err := factory.Build(graph.NodeDefinition{
		ID:   "script_fs",
		Type: "script",
		Config: map[string]any{
			"source": `board.setVar("script_file", fs.read("docs/script.txt"));`,
		},
	})
	if err != nil {
		t.Fatalf("Build script: %v", err)
	}

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: ctx}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("script_file"); got != "from-script-workspace" {
		t.Fatalf("script_file = %v, want from-script-workspace", got)
	}
}

func TestRegister_ScriptFSFallbackInjectsFSBridge(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	if err := ws.Write(ctx, "docs/fallback.txt", []byte("from-fallback-workspace")); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		Workspace:     ws,
		ScriptFS: fstest.MapFS{
			"custom.js": {Data: []byte(`board.setVar("fallback_file", fs.read("docs/fallback.txt"));`)},
		},
	})
	n, err := factory.Build(graph.NodeDefinition{
		ID:   "fallback_fs",
		Type: "custom",
	})
	if err != nil {
		t.Fatalf("Build fallback: %v", err)
	}

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: ctx}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("fallback_file"); got != "from-fallback-workspace" {
		t.Fatalf("fallback_file = %v, want from-fallback-workspace", got)
	}
}

func TestRegister_PreservesExistingFallbackAfterScriptFSMiss(t *testing.T) {
	factory := nodepkg.NewFactory()
	var fallbackCalled bool
	var fallbackDef graph.NodeDefinition
	factory.SetFallback(func(def graph.NodeDefinition) (graph.Node, error) {
		fallbackCalled = true
		fallbackDef = def
		return graph.NewPassthroughNode(def.ID, "legacy"), nil
	})
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		ScriptFS:      fstest.MapFS{},
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "legacy_node",
		Type: "legacy_type",
	})
	if err != nil {
		t.Fatalf("Build legacy fallback: %v", err)
	}
	if !fallbackCalled {
		t.Fatal("expected existing fallback to be called")
	}
	if fallbackDef.Type != "legacy_type" {
		t.Fatalf("fallback Type = %q, want legacy_type", fallbackDef.Type)
	}
	if n.ID() != "legacy_node" || n.Type() != "legacy" {
		t.Fatalf("fallback node = (%q, %q), want (legacy_node, legacy)", n.ID(), n.Type())
	}
}

func TestRegister_ScriptFSHitDoesNotCallExistingFallbackWhenRuntimeMissing(t *testing.T) {
	factory := nodepkg.NewFactory()
	var fallbackCalled bool
	factory.SetFallback(func(def graph.NodeDefinition) (graph.Node, error) {
		fallbackCalled = true
		return graph.NewPassthroughNode(def.ID, "legacy"), nil
	})
	Register(factory, Deps{
		ScriptFS: fstest.MapFS{
			"custom.js": {Data: []byte(`board.setVar("source", "scriptfs");`)},
		},
	})

	_, err := factory.Build(graph.NodeDefinition{
		ID:   "scriptfs_node",
		Type: "custom",
	})
	if err == nil || !strings.Contains(err.Error(), "script runtime not configured") {
		t.Fatalf("Build error = %v, want script runtime validation error", err)
	}
	if fallbackCalled {
		t.Fatal("existing fallback should not be called when ScriptFS resolves the type")
	}
}

func TestRegister_ScriptFSReadErrorDoesNotCallExistingFallback(t *testing.T) {
	factory := nodepkg.NewFactory()
	var fallbackCalled bool
	factory.SetFallback(func(def graph.NodeDefinition) (graph.Node, error) {
		fallbackCalled = true
		return graph.NewPassthroughNode(def.ID, "legacy"), nil
	})
	readErr := errors.New("read denied")
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		ScriptFS:      readFileErrorFS{err: readErr},
	})

	_, err := factory.Build(graph.NodeDefinition{
		ID:   "bad_scriptfs",
		Type: "custom",
	})
	if !errdefs.IsInternal(err) {
		t.Fatalf("Build error = %v, want Internal", err)
	}
	for _, want := range []string{"bad_scriptfs", "custom", "custom.js", "read denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Build error = %q, want substring %q", err.Error(), want)
		}
	}
	if fallbackCalled {
		t.Fatal("existing fallback should not be called when ScriptFS read fails")
	}
}

func TestRegister_ReRegisterDoesNotPreservePreviousScriptFSFallback(t *testing.T) {
	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		ScriptFS: fstest.MapFS{
			"legacy.js": {Data: []byte(`board.setVar("legacy", true);`)},
		},
	})
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		ScriptFS:      fstest.MapFS{},
	})

	_, err := factory.Build(graph.NodeDefinition{
		ID:   "legacy",
		Type: "legacy",
	})
	if err == nil || !strings.Contains(err.Error(), `unknown node type "legacy"`) {
		t.Fatalf("Build error = %v, want unknown node type after re-register", err)
	}
}

func TestRegister_ReRegisterStillPreservesExternalFallback(t *testing.T) {
	factory := nodepkg.NewFactory()
	var fallbackCalls int
	factory.SetFallback(func(def graph.NodeDefinition) (graph.Node, error) {
		fallbackCalls++
		return graph.NewPassthroughNode(def.ID, "external"), nil
	})
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		ScriptFS:      fstest.MapFS{},
	})
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		ScriptFS:      fstest.MapFS{},
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "external_node",
		Type: "external_type",
	})
	if err != nil {
		t.Fatalf("Build external fallback: %v", err)
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallbackCalls)
	}
	if n.ID() != "external_node" || n.Type() != "external" {
		t.Fatalf("fallback node = (%q, %q), want (external_node, external)", n.ID(), n.Type())
	}
}

func TestRegister_ExternalFallbackInstalledAfterScriptFallbackIsPreservedOnReRegister(t *testing.T) {
	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		ScriptFS: fstest.MapFS{
			"old.js": {Data: []byte(`board.setVar("old", true);`)},
		},
	})

	var fallbackCalls int
	factory.SetFallback(func(def graph.NodeDefinition) (graph.Node, error) {
		fallbackCalls++
		return graph.NewPassthroughNode(def.ID, "late_external"), nil
	})
	Register(factory, Deps{
		ScriptRuntime: jsrt.New(jsrt.WithPoolSize(1)),
		ScriptFS:      fstest.MapFS{},
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "late_external_node",
		Type: "old",
	})
	if err != nil {
		t.Fatalf("Build late external fallback: %v", err)
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallbackCalls)
	}
	if n.ID() != "late_external_node" || n.Type() != "late_external" {
		t.Fatalf("fallback node = (%q, %q), want (late_external_node, late_external)", n.ID(), n.Type())
	}
}

func TestRegister_ContextFilesOnlyBuildsWithoutCommandRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{ScriptRuntime: rt})

	_, err := factory.Build(graph.NodeDefinition{
		ID:   "ctx_files",
		Type: "context",
		Config: map[string]any{
			"files": []any{
				map[string]any{"path": "README.md"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build files-only context without CommandRunner: %v", err)
	}
}

func TestRegister_GateWithoutCommandsBuildsWithoutCommandRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{ScriptRuntime: rt})

	_, err := factory.Build(graph.NodeDefinition{
		ID:     "gate_empty",
		Type:   "gate",
		Config: map[string]any{"commands": []any{}},
	})
	if err != nil {
		t.Fatalf("Build commandless gate without CommandRunner: %v", err)
	}
}

func TestRegister_GateCommandsInjectCommandRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: rt,
		CommandRunner: contextCommandRunner{stdout: "gate runner output\n"},
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "gate_commands",
		Type: "gate",
		Config: map[string]any{
			"commands": []any{"echo gate"},
		},
	})
	if err != nil {
		t.Fatalf("Build gate with commands: %v", err)
	}

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: context.Background()}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("gate_result"); got != "pass" {
		t.Fatalf("gate_result = %v, want pass", got)
	}
	if got, _ := board.GetVar("gate_result_output"); got != "gate runner output\n" {
		t.Fatalf("gate_result_output = %v, want runner stdout", got)
	}
}

func TestRegister_ContextCommandsInjectCommandRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{
		ScriptRuntime: rt,
		CommandRunner: contextCommandRunner{stdout: "hello from runner\n"},
	})

	n, err := factory.Build(graph.NodeDefinition{
		ID:   "ctx_commands",
		Type: "context",
		Config: map[string]any{
			"commands": []any{
				map[string]any{"command": "echo hello", "state_key": "cmd_out"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build context with commands: %v", err)
	}

	board := graph.NewBoard()
	if err := n.ExecuteBoard(graph.ExecutionContext{Context: context.Background()}, board); err != nil {
		t.Fatalf("ExecuteBoard: %v", err)
	}
	if got, _ := board.GetVar("cmd_out"); got != "hello from runner\n" {
		t.Fatalf("cmd_out = %v, want command stdout", got)
	}
}

func TestRegister_BuiltinCommandsRequireCommandRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	factory := nodepkg.NewFactory()
	Register(factory, Deps{ScriptRuntime: rt})

	for _, nodeType := range []string{"context", "gate"} {
		t.Run(nodeType, func(t *testing.T) {
			_, err := factory.Build(graph.NodeDefinition{
				ID:     nodeType + "1",
				Type:   nodeType,
				Config: map[string]any{"commands": []any{"echo hi"}},
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("Build error = %v, want Validation", err)
			}
		})
	}
}
