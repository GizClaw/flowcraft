package workspace_test

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// TestCommandRunner_DeprecationAliases is a sanity check that the
// workspace shim (CommandRunner / LocalCommandRunner / NoopCommandRunner
// / ScopedCommandRunner / WithMaxOutput / NewLocalCommandRunner /
// NewScopedCommandRunner) still lets callers Exec successfully through
// the alias chain to sdk/sandbox. Substantive coverage lives in the
// sdk/sandbox package tests; this only proves the alias chain compiles
// and dispatches.
func TestCommandRunner_DeprecationAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	var runner workspace.CommandRunner = workspace.NewLocalCommandRunner(t.TempDir(), workspace.WithMaxOutput(1024))

	result, err := runner.Exec(context.Background(), "echo", []string{"hello"}, workspace.ExecOptions{})
	if err != nil {
		t.Fatalf("alias chain Exec failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello") {
		t.Fatalf("Stdout = %q, want substring 'hello'", result.Stdout)
	}

	// Noop alias still satisfies the interface and returns empty result.
	var noop workspace.CommandRunner = workspace.NoopCommandRunner{}
	if res, err := noop.Exec(context.Background(), "anything", nil, workspace.ExecOptions{}); err != nil || res.ExitCode != 0 {
		t.Fatalf("NoopCommandRunner alias broken: res=%+v err=%v", res, err)
	}

	// Scoped alias wraps sandbox.AllowCommands and rejects unknown commands.
	scoped := workspace.NewScopedCommandRunner(workspace.NoopCommandRunner{}, []string{"echo"})
	if _, err := scoped.Exec(context.Background(), "rm", nil, workspace.ExecOptions{}); err == nil {
		t.Fatal("scoped alias should block command outside whitelist")
	}
	if _, err := scoped.Exec(context.Background(), "echo", nil, workspace.ExecOptions{}); err != nil {
		t.Fatalf("scoped alias should allow whitelisted command: %v", err)
	}

	// ExecOptions and ExecResult are aliases to sandbox types — verify
	// they're interchangeable at the type level.
	var _ sandbox.ExecOptions = workspace.ExecOptions{}
	var _ sandbox.ExecResult = workspace.ExecResult{}
}
