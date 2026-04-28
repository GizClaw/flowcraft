package bindings_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// fakeCommandRunner is a CommandRunner stub that returns canned output
// without actually shelling out — keeps these tests hermetic and fast.
type fakeCommandRunner struct {
	stdout   string
	stderr   string
	exitCode int
}

func (f *fakeCommandRunner) Exec(_ context.Context, _ string, _ []string, _ workspace.ExecOptions) (*workspace.ExecResult, error) {
	return &workspace.ExecResult{Stdout: f.stdout, Stderr: f.stderr, ExitCode: f.exitCode}, nil
}

// errorCommandRunner always fails — used to verify the bridge's error
// translation branch (Go err → exit_code -1 + stderr).
type errorCommandRunner struct{ err error }

func (e *errorCommandRunner) Exec(_ context.Context, _ string, _ []string, _ workspace.ExecOptions) (*workspace.ExecResult, error) {
	return nil, e.err
}

var errFakeRunnerDown = fakeRunnerErr("runner-down")

type fakeRunnerErr string

func (e fakeRunnerErr) Error() string { return string(e) }

func TestShellBridge_AllowList_Allowed(t *testing.T) {
	runner := &fakeCommandRunner{
		stdout:   "ok",
		exitCode: 0,
	}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()

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
	board := engine.NewBoard()

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
	board := engine.NewBoard()

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
	board := engine.NewBoard()

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

func TestShellBridge_AllowList_FullPath(t *testing.T) {
	runner := &fakeCommandRunner{stdout: "ok", exitCode: 0}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()

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

func TestShellBridge_EmptyCommand(t *testing.T) {
	// Empty / whitespace-only command must reject with exit_code -1 and a
	// human-readable stderr — confirms the strings.Fields(cmd) early-return
	// branch in NewShellBridge.exec.
	runner := &fakeCommandRunner{stdout: "ok", exitCode: 0}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewShellBridge(runner),
	)
	_, err := rt.Exec(context.Background(), "shell-empty", `
		var r1 = shell.exec("");
		if (r1.exit_code !== -1) throw new Error("empty cmd should reject, got " + r1.exit_code);
		if (r1.stderr.indexOf("empty") === -1) throw new Error("stderr: " + r1.stderr);

		var r2 = shell.exec("   ");
		if (r2.exit_code !== -1) throw new Error("whitespace cmd should reject");

		board.setVar("ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := board.GetVar("ok"); v != true {
		t.Fatal("script did not complete assertions")
	}
}

func TestShellBridge_RunnerError_SurfacedAsExitMinusOne(t *testing.T) {
	// A runner returning a Go error must surface to the script as
	// exit_code: -1 with stderr carrying the error message — confirms the
	// NewShellBridge.exec error branch (vs. silently dropping err).
	runner := &errorCommandRunner{err: errFakeRunnerDown}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewShellBridge(runner),
	)
	_, err := rt.Exec(context.Background(), "shell-err", `
		var r = shell.exec("anything");
		if (r.exit_code !== -1) throw new Error("expected -1, got " + r.exit_code);
		if (r.stderr.indexOf("runner-down") === -1) throw new Error("stderr should carry runner err: " + r.stderr);
		board.setVar("ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := board.GetVar("ok"); v != true {
		t.Fatal("script did not complete assertions")
	}
}

func TestShellBridge_AllowList_FullPath_Blocked(t *testing.T) {
	runner := &fakeCommandRunner{stdout: "ok", exitCode: 0}
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := engine.NewBoard()

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
