package bindings_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
)

// RuntimeBinding is normally wired by scriptnode, not user code, so these
// tests assemble the same shape manually: parent env -> "runtime" binding ->
// child script. They confirm four contracts:
//   1. parent bindings (e.g. board) are visible inside the child script.
//   2. signals raised in the child surface to the Go caller of execScript.
//   3. errors thrown in the child propagate as Go errors back to the parent.
//   4. the second arg of execScript becomes the child's `config` global.

// envWithRuntime builds a parent script.Env containing every supplied bridge
// plus a "runtime" binding wired to the same runtime, mimicking what
// scriptnode does internally. Returned env is ready to be passed straight to
// rt.Exec.
func envWithRuntime(ctx context.Context, rt script.Runtime, fns ...bindings.BindingFunc) *script.Env {
	host := map[string]any{}
	for _, fn := range fns {
		k, v := fn(ctx)
		host[k] = v
	}
	host["runtime"] = bindings.RuntimeBinding(ctx, rt, host)
	return &script.Env{Bindings: host}
}

func TestRuntimeBinding_ChildInheritsParentBindings(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	board := engine.NewBoard()
	env := envWithRuntime(context.Background(), rt, bindings.NewBoardBridge(board))

	_, err := rt.Exec(context.Background(), "parent", `
		// Child script touches the inherited "board" global to prove
		// parentBindings landed in its env.
		var sig = runtime.execScript('board.setVar("from_child", 99);', null);
		if (sig !== null) throw new Error("expected null signal, got " + JSON.stringify(sig));
	`, env)
	if err != nil {
		t.Fatalf("parent exec failed: %v", err)
	}
	if v, _ := board.GetVar("from_child"); v != int64(99) {
		t.Fatalf("child failed to mutate inherited board: got %v (%T)", v, v)
	}
}

func TestRuntimeBinding_ChildSignalSurfaces(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	board := engine.NewBoard()
	env := envWithRuntime(context.Background(), rt, bindings.NewBoardBridge(board))

	// goja exposes Go struct fields by their Go name (no FieldNameMapper is
	// installed in jsrt), so script.Signal.Type/Message are accessed as
	// sig.Type / sig.Message — confirming that contract from the script side.
	_, err := rt.Exec(context.Background(), "parent", `
		var sig = runtime.execScript('signal.interrupt("need approval")', null);
		if (sig === null) throw new Error("expected non-null signal");
		if (sig.Type !== "interrupt") throw new Error("Type = " + sig.Type);
		if (sig.Message !== "need approval") throw new Error("Message = " + sig.Message);
		board.setVar("ok", true);
	`, env)
	if err != nil {
		t.Fatalf("parent exec failed: %v", err)
	}
	if v, _ := board.GetVar("ok"); v != true {
		t.Fatal("script did not complete signal assertions")
	}
}

func TestRuntimeBinding_ChildErrorPropagates(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	board := engine.NewBoard()
	env := envWithRuntime(context.Background(), rt, bindings.NewBoardBridge(board))

	_, err := rt.Exec(context.Background(), "parent", `
		try {
			runtime.execScript('throw new Error("boom from child")', null);
		} catch (e) {
			if (String(e).indexOf("boom from child") === -1) {
				throw new Error("child error did not surface: " + e);
			}
			board.setVar("caught", true);
			return;
		}
		throw new Error("expected execScript to throw on child error");
	`, env)
	if err != nil {
		t.Fatalf("parent exec failed: %v", err)
	}
	if v, _ := board.GetVar("caught"); v != true {
		t.Fatal("script did not catch propagated child error")
	}
}

func TestRuntimeBinding_ChildReceivesConfig(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	board := engine.NewBoard()
	env := envWithRuntime(context.Background(), rt, bindings.NewBoardBridge(board))

	_, err := rt.Exec(context.Background(), "parent", `
		runtime.execScript(
			'if (config.greeting !== "hi") throw new Error("config not injected: " + config.greeting); board.setVar("g", config.greeting);',
			{ greeting: "hi" }
		);
	`, env)
	if err != nil {
		t.Fatalf("parent exec failed: %v", err)
	}
	if v, _ := board.GetVar("g"); v != "hi" {
		t.Fatalf("child did not see config.greeting: got %v", v)
	}
}
