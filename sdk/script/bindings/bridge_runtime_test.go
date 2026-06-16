package bindings_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/script/luart"
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
	return script.NewEnvBuilder(nil).
		Add(fns...).
		AddLate(bindings.NewRuntimeBridge(rt)).
		Build(ctx)
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

func TestRuntimeBinding_PoolSizeOneRejectsNestedExec(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()
	env := envWithRuntime(context.Background(), rt, bindings.NewBoardBridge(board))

	_, err := rt.Exec(context.Background(), "parent", `
		var sig = runtime.execScript('board.setVar("from_child", 99);', null);
		if (sig === null) throw new Error("expected rejection signal");
		if (sig.Type !== "error") throw new Error("Type = " + sig.Type);
		if (sig.Kind !== "not_available") throw new Error("Kind = " + sig.Kind);
		board.setVar("rejected", true);
	`, env)
	if err != nil {
		t.Fatalf("parent exec failed: %v", err)
	}
	if v, _ := board.GetVar("rejected"); v != true {
		t.Fatal("script did not observe nested exec rejection")
	}
	if v, ok := board.GetVar("from_child"); ok && v != nil {
		t.Fatalf("child script should not have run, got from_child=%v", v)
	}
}

func TestRuntimeBinding_LuaPoolSizeOneRejectsNestedExec(t *testing.T) {
	rt := luart.New(luart.WithPoolSize(1))
	t.Cleanup(func() { _ = rt.Close() })
	board := engine.NewBoard()
	env := envWithRuntime(context.Background(), rt, bindings.NewBoardBridge(board))

	_, err := rt.Exec(context.Background(), "parent", `
		local sig = runtime.execScript('board.setVar("from_child", 99)', nil)
		if sig == nil then error("expected rejection signal") end
		if sig.type ~= "error" then error("type = " .. tostring(sig.type)) end
		if sig.kind ~= "not_available" then error("kind = " .. tostring(sig.kind)) end
		board.setVar("rejected_lua", true)
	`, env)
	if err != nil {
		t.Fatalf("parent exec failed: %v", err)
	}
	if v, _ := board.GetVar("rejected_lua"); v != true {
		t.Fatal("script did not observe nested exec rejection")
	}
	if v, ok := board.GetVar("from_child"); ok && v != nil {
		t.Fatalf("child script should not have run, got from_child=%v", v)
	}
}

func TestRuntimeBinding_ConcurrentParentsFailFastWhenPoolBusy(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	board := engine.NewBoard()
	barrier := newRuntimeBarrier(2)
	env := envWithRuntime(
		context.Background(),
		rt,
		bindings.NewBoardBridge(board),
		func(context.Context) (string, any) {
			return "barrier", map[string]any{
				"beforeNested": barrier.before,
				"afterNested":  barrier.after,
			}
		},
	)

	parent := `
		barrier.beforeNested();
		var sig = runtime.execScript('board.setVar("from_child", true);', null);
		if (sig === null) throw new Error("expected rejection signal");
		if (sig.Type !== "error") throw new Error("Type = " + sig.Type);
		if (sig.Kind !== "not_available") throw new Error("Kind = " + sig.Kind);
		barrier.afterNested();
	`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := rt.Exec(ctx, "parent", parent, env)
			errCh <- err
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("parent %d exec failed: %v", i, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent nested exec did not fail fast")
		}
	}
	if v, ok := board.GetVar("from_child"); ok && v != nil {
		t.Fatalf("child script should not have run, got from_child=%v", v)
	}
}

type runtimeBarrier struct {
	want int

	mu          sync.Mutex
	beforeCount int
	afterCount  int
	beforeCh    chan struct{}
	afterCh     chan struct{}
}

func newRuntimeBarrier(want int) *runtimeBarrier {
	return &runtimeBarrier{
		want:     want,
		beforeCh: make(chan struct{}),
		afterCh:  make(chan struct{}),
	}
}

func (b *runtimeBarrier) before() error {
	return b.wait(&b.beforeCount, b.beforeCh)
}

func (b *runtimeBarrier) after() error {
	return b.wait(&b.afterCount, b.afterCh)
}

func (b *runtimeBarrier) wait(count *int, ch chan struct{}) error {
	b.mu.Lock()
	*count = *count + 1
	if *count == b.want {
		close(ch)
	}
	b.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("barrier timeout")
	}
}
