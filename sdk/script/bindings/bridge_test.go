package bindings_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestBoardBridge(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()
	board.SetVar("x", 10)

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	_, err := rt.Exec(context.Background(), "board", `
		var val = board.getVar("x");
		if (val !== 10) throw new Error("expected 10, got " + val);
		board.setVar("y", val * 2);
		if (!board.hasVar("x")) throw new Error("hasVar failed");
		if (board.hasVar("z")) throw new Error("hasVar false positive");
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	y, ok := board.GetVar("y")
	if !ok {
		t.Fatal("board should have 'y'")
	}
	if y != int64(20) {
		t.Fatalf("y = %v (type %T), want 20", y, y)
	}
}

func TestExprBridge(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board), bindings.NewExprBridge())
	_, err := rt.Exec(context.Background(), "expr", `
		var result = expr.eval("a + b", {a: 3, b: 4});
		if (result !== 7) throw new Error("expected 7, got " + result);
		board.setVar("expr_result", result);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, _ := board.GetVar("expr_result")
	if v != int64(7) {
		t.Fatalf("expr_result = %v (type %T)", v, v)
	}
}

func TestStreamBridge(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	var captured []workflow.StreamEvent
	cb := func(e workflow.StreamEvent) {
		captured = append(captured, e)
	}

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewStreamBridge(cb, "node1"))
	_, err := rt.Exec(context.Background(), "stream", `
		stream.emit("token", {content: "hello"});
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d events, want 1", len(captured))
	}
	if captured[0].Type != "token" {
		t.Fatalf("event type = %q", captured[0].Type)
	}
	if captured[0].NodeID != "node1" {
		t.Fatalf("event nodeID = %q", captured[0].NodeID)
	}
}

func TestStreamBridge_NilCallback(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	env := bindings.BuildEnv(context.Background(), nil, bindings.NewStreamBridge(nil, "node1"))
	_, err := rt.Exec(context.Background(), "stream-nil", `
		stream.emit("token", {content: "hello"});
	`, env)
	if err != nil {
		t.Fatalf("unexpected error with nil callback: %v", err)
	}
}

func TestSignalBridge_Error(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	sig, err := rt.Exec(context.Background(), "sig-err", `signal.error("bad input")`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig == nil || sig.Type != "error" {
		t.Fatalf("signal = %+v, want error type", sig)
	}
}

func TestSignalBridge_Done(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	sig, err := rt.Exec(context.Background(), "sig-done", `signal.done()`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig == nil || sig.Type != "done" {
		t.Fatalf("signal = %+v, want done type", sig)
	}
}

func TestShellBridge_AllowList_Allowed(t *testing.T) {
	runner := &fakeCommandRunner{
		stdout:   "ok",
		exitCode: 0,
	}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewShellBridge(runner, bindings.WithAllowedCommands("echo", "cat")),
	)
	_, err := rt.Exec(context.Background(), "shell-allow", `
		var result = shell.exec("echo", "hello");
		if (result.exit_code !== 0) throw new Error("expected exit 0, got " + result.exit_code);
		if (result.stdout !== "ok") throw new Error("expected 'ok', got " + result.stdout);
		board.setVar("shell_ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("shell_ok")
	if v != true {
		t.Fatal("shell command should have succeeded")
	}
}

func TestShellBridge_AllowList_Blocked(t *testing.T) {
	runner := &fakeCommandRunner{stdout: "ok", exitCode: 0}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewShellBridge(runner, bindings.WithAllowedCommands("echo")),
	)
	_, err := rt.Exec(context.Background(), "shell-block", `
		var result = shell.exec("rm -rf /");
		if (result.exit_code !== -1) throw new Error("expected rejection, got exit " + result.exit_code);
		if (result.stderr.indexOf("not allowed") === -1) throw new Error("expected 'not allowed' in stderr: " + result.stderr);
		board.setVar("blocked", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("blocked")
	if v != true {
		t.Fatal("blocked command check should have passed")
	}
}

func TestShellBridge_NoAllowList(t *testing.T) {
	runner := &fakeCommandRunner{stdout: "ok", exitCode: 0}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewShellBridge(runner),
	)
	_, err := rt.Exec(context.Background(), "shell-nolist", `
		var result = shell.exec("anything");
		if (result.exit_code !== 0) throw new Error("expected exit 0");
		board.setVar("no_list_ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("no_list_ok")
	if v != true {
		t.Fatal("without allow list, any command should pass")
	}
}

func TestShellBridge_NilRunner(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewShellBridge(nil),
	)
	_, err := rt.Exec(context.Background(), "shell-nil", `
		var result = shell.exec("echo hello");
		if (result.exit_code !== -1) throw new Error("expected -1 with nil runner");
		board.setVar("nil_ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("nil_ok")
	if v != true {
		t.Fatal("nil runner should return exit_code -1")
	}
}

func TestExprBridge_CachedResults(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board), bindings.NewExprBridge())
	_, err := rt.Exec(context.Background(), "expr-cache", `
		var r1 = expr.eval("x * 2", {x: 5});
		var r2 = expr.eval("x * 2", {x: 10});
		if (r1 !== 10) throw new Error("first eval: " + r1);
		if (r2 !== 20) throw new Error("second eval (same expr, diff env): " + r2);
		board.setVar("cache_ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("cache_ok")
	if v != true {
		t.Fatal("cached expr results should be correct")
	}
}

func TestExprBridge_InvalidExpression(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board), bindings.NewExprBridge())
	_, err := rt.Exec(context.Background(), "expr-invalid", `
		try {
			expr.eval("!!!invalid", {});
			throw new Error("should have failed");
		} catch(e) {
			board.setVar("caught", true);
		}
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("caught")
	if v != true {
		t.Fatal("invalid expression should throw error")
	}
}

// --- fake CommandRunner for testing ---

type fakeCommandRunner struct {
	stdout   string
	stderr   string
	exitCode int
}

func (f *fakeCommandRunner) Exec(_ context.Context, _ string, _ []string, _ workspace.ExecOptions) (*workspace.ExecResult, error) {
	return &workspace.ExecResult{Stdout: f.stdout, Stderr: f.stderr, ExitCode: f.exitCode}, nil
}

func TestShellBridge_AllowList_FullPath(t *testing.T) {
	runner := &fakeCommandRunner{stdout: "ok", exitCode: 0}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewShellBridge(runner, bindings.WithAllowedCommands("echo")),
	)
	_, err := rt.Exec(context.Background(), "shell-fullpath", `
		var result = shell.exec("/usr/bin/echo", "hello");
		if (result.exit_code !== 0) throw new Error("expected exit 0, got " + result.exit_code);
		board.setVar("fullpath_ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("fullpath_ok")
	if v != true {
		t.Fatal("full-path command matching via filepath.Base should work")
	}
}

func TestShellBridge_AllowList_FullPath_Blocked(t *testing.T) {
	runner := &fakeCommandRunner{stdout: "ok", exitCode: 0}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := workflow.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewShellBridge(runner, bindings.WithAllowedCommands("echo")),
	)
	_, err := rt.Exec(context.Background(), "shell-fullpath-block", `
		var result = shell.exec("/usr/bin/rm -rf /");
		if (result.exit_code !== -1) throw new Error("expected rejection");
		if (result.stderr.indexOf("not allowed") === -1) throw new Error("expected 'not allowed' in stderr");
		board.setVar("blocked_ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := board.GetVar("blocked_ok")
	if v != true {
		t.Fatal("full-path command not in allow list should be blocked")
	}
}
