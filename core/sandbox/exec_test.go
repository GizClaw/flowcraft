package sandbox_test

import (
	"context"
	"runtime"
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

// skipOnWindows guards tests that exercise the unix-only session
// machinery of core/sandbox/local (pty / process groups / signals).
// The windows backend has its own integration coverage in
// core/sandbox/windows.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sandbox/local sessions require a unix platform")
	}
}

func TestExecEcho(t *testing.T) {
	skipOnWindows(t)
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
	skipOnWindows(t)
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

// fd0Check reports what the child's stdin actually is: a pipe means a
// session wired input, chardev is the OS default (/dev/null). Programs
// that branch on stdin (`rg` with no path operand, `cat`, `test -p`)
// read this shape, which is why it is asserted instead of "did the
// write succeed".
const fd0Check = `f=/dev/fd/0; [ -e "$f" ] || f=/proc/self/fd/0; ` +
	`if [ -p "$f" ]; then echo pipe; elif [ -c "$f" ]; then echo chardev; else echo other; fi`

func fd0Shape(t *testing.T, opts sandbox.ExecOptions) string {
	t.Helper()
	result, err := sandbox.Exec(
		context.Background(), localRunner(t), "sh", []string{"-c", fd0Check}, opts)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("fd0 probe exited %d: %s", result.ExitCode, result.Stderr)
	}
	return strings.TrimSpace(result.Stdout)
}

// TestExecWithoutStdinGetsDevNull is the regression for the never-ending
// empty pipe: with no input declared the child must see the OS default,
// so `rg pattern` searches its operands instead of that pipe.
func TestExecWithoutStdinGetsDevNull(t *testing.T) {
	skipOnWindows(t)
	if got := fd0Shape(t, sandbox.ExecOptions{}); got != "chardev" {
		t.Fatalf("fd 0 = %q, want chardev (/dev/null)", got)
	}
}

// TestExecEmptyStdinIsNoStdin pins the caller habit that says "no input"
// with an empty slice: it must not buy a pipe either.
func TestExecEmptyStdinIsNoStdin(t *testing.T) {
	skipOnWindows(t)
	if got := fd0Shape(t, sandbox.ExecOptions{Stdin: []byte{}}); got != "chardev" {
		t.Fatalf("fd 0 with empty Stdin = %q, want chardev (/dev/null)", got)
	}
}

// TestExecWithStdinKeepsPipe guards the other direction: declared bytes
// still reach the child through a pipe.
func TestExecWithStdinKeepsPipe(t *testing.T) {
	skipOnWindows(t)
	if got := fd0Shape(t, sandbox.ExecOptions{Stdin: []byte("x")}); got != "pipe" {
		t.Fatalf("fd 0 with Stdin = %q, want pipe", got)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	skipOnWindows(t)
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
	skipOnWindows(t)
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

func TestExecTruncatesLargeFastOutput(t *testing.T) {
	skipOnWindows(t)
	// Regression: the session ring must not trim to the caller's small
	// result cap before Exec drains it, otherwise a fast writer turns
	// truncation into ErrSequenceGap.
	result, err := sandbox.Exec(
		context.Background(), localRunner(t), "sh",
		[]string{"-c", "yes x | head -c 300000"},
		sandbox.ExecOptions{Resources: sandbox.ResourceLimits{MaxOutputBytes: 100}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(result.Stdout) != 100 {
		t.Fatalf("stdout length = %d, want 100", len(result.Stdout))
	}
	if !strings.HasPrefix(result.Stdout, "x") {
		t.Fatalf("stdout = %q, want 'x' prefix", result.Stdout)
	}
}

func TestExecTimeout(t *testing.T) {
	skipOnWindows(t)
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
	skipOnWindows(t)
	result, err := localRunner(t).Exec(
		context.Background(), "echo", []string{"ok"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "ok\n" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}
