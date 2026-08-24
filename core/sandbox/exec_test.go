//go:build unix

package sandbox_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
)

var _ sandbox.Runner = (*local.Runner)(nil)

func localRunner(t *testing.T) *local.Runner {
	t.Helper()
	return local.New(t.TempDir())
}

func TestExecEcho(t *testing.T) {
	result, err := sandbox.Exec(
		context.Background(), localRunner(t), "echo", []string{"hi"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "hi\n" {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecStdin(t *testing.T) {
	result, err := sandbox.Exec(
		context.Background(), localRunner(t), "cat", nil,
		sandbox.ExecOptions{Stdin: []byte("hello")})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "hello" {
		t.Fatalf("stdout = %q, want hello", result.Stdout)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	result, err := sandbox.Exec(
		context.Background(), localRunner(t), "sh",
		[]string{"-c", "exit 3"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", result.ExitCode)
	}
}

func TestExecTruncatesOutput(t *testing.T) {
	result, err := sandbox.Exec(
		context.Background(), localRunner(t), "sh",
		[]string{"-c", "printf 'abcdefghijklmnopqrstuvwxyz'"},
		sandbox.ExecOptions{Resources: sandbox.ResourceLimits{MaxOutputBytes: 10}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "abcdefghij" {
		t.Fatalf("stdout = %q, want first 10 bytes", result.Stdout)
	}
}

func TestExecTimeout(t *testing.T) {
	_, err := sandbox.Exec(
		context.Background(), localRunner(t), "sleep", []string{"5"},
		sandbox.ExecOptions{Timeout: 200 * time.Millisecond})
	if err == nil {
		t.Fatal("Exec unexpectedly succeeded")
	}
	if !errdefs.IsTimeout(err) && !strings.Contains(err.Error(), "Timeout") {
		t.Fatalf("Exec error = %v, want timeout", err)
	}
}

func TestRunnerExecMethod(t *testing.T) {
	result, err := localRunner(t).Exec(
		context.Background(), "echo", []string{"ok"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "ok\n" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}
