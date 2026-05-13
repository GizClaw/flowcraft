package sandbox_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// recordingRunner records the most recent Exec call so AllowCommands
// tests can prove that allowed calls do pass through to the inner Runner
// (and blocked calls do not).
type recordingRunner struct {
	called bool
	cmd    string
}

func (r *recordingRunner) Exec(_ context.Context, cmd string, _ []string, _ sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	r.called = true
	r.cmd = cmd
	return &sandbox.ExecResult{Stdout: "ok"}, nil
}

func TestAllowCommands_Pass(t *testing.T) {
	inner := &recordingRunner{}
	r := sandbox.AllowCommands(inner, []string{"ls", "cat", "echo"})

	result, err := r.Exec(context.Background(), "ls", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("allowed command should succeed: %v", err)
	}
	if !inner.called {
		t.Fatal("allowed command should reach inner runner")
	}
	if inner.cmd != "ls" {
		t.Fatalf("inner.cmd = %q, want 'ls'", inner.cmd)
	}
	if result.Stdout != "ok" {
		t.Fatalf("result not passed through, got Stdout = %q", result.Stdout)
	}
}

func TestAllowCommands_Reject(t *testing.T) {
	inner := &recordingRunner{}
	r := sandbox.AllowCommands(inner, []string{"ls", "cat"})

	_, err := r.Exec(context.Background(), "rm", []string{"-rf", "/"}, sandbox.ExecOptions{})
	if err == nil {
		t.Fatal("blocked command should fail")
	}
	if !strings.Contains(err.Error(), "whitelist") {
		t.Fatalf("error should mention whitelist, got: %v", err)
	}
	if inner.called {
		t.Fatal("blocked command must not reach inner runner")
	}
}

func TestAllowCommands_EmptyWhitelist(t *testing.T) {
	inner := &recordingRunner{}
	r := sandbox.AllowCommands(inner, nil)

	_, err := r.Exec(context.Background(), "ls", nil, sandbox.ExecOptions{})
	if err == nil {
		t.Fatal("empty whitelist should block all commands")
	}
	if inner.called {
		t.Fatal("empty whitelist must not reach inner runner")
	}
}

func TestNoopRunner(t *testing.T) {
	result, err := sandbox.NoopRunner{}.Exec(context.Background(), "anything", []string{"arg"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("NoopRunner should not error: %v", err)
	}
	if result == nil {
		t.Fatal("NoopRunner should return non-nil ExecResult")
	}
	if result.ExitCode != 0 || result.Stdout != "" || result.Stderr != "" {
		t.Fatalf("NoopRunner should return empty result, got: %+v", *result)
	}
}
