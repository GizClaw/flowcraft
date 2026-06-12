package scriptnode

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/sdk/graph"
	nodepkg "github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
)

func testValueBridge(value string) bindings.BindingFunc {
	return func(context.Context) (string, any) {
		return "extra", map[string]any{
			"value": func() string { return value },
		}
	}
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
