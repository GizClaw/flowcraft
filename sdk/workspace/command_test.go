package workspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalCommandRunner_BasicExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	runner := NewLocalCommandRunner(dir)

	result, err := runner.Exec(context.Background(), "echo", []string{"hello"}, ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Fatalf("Stdout = %q", result.Stdout)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d", result.ExitCode)
	}
}

func TestLocalCommandRunner_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	runner := NewLocalCommandRunner(dir)

	result, err := runner.Exec(context.Background(), "sh", []string{"-c", "exit 42"}, ExecOptions{})
	if err != nil {
		t.Fatalf("non-zero exit should not return error: %v", err)
	}
	if result.ExitCode != 42 {
		t.Fatalf("ExitCode = %d, want 42", result.ExitCode)
	}
}

func TestLocalCommandRunner_EnvInheritance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	runner := NewLocalCommandRunner(dir)

	result, err := runner.Exec(context.Background(), "sh", []string{"-c", "echo $HOME"}, ExecOptions{
		Env: map[string]string{"CUSTOM_VAR": "custom_value"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) == "" {
		t.Fatal("$HOME should be inherited from parent environment")
	}

	result, err = runner.Exec(context.Background(), "sh", []string{"-c", "echo $CUSTOM_VAR"}, ExecOptions{
		Env: map[string]string{"CUSTOM_VAR": "hello_world"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "hello_world" {
		t.Fatalf("CUSTOM_VAR = %q, want 'hello_world'", strings.TrimSpace(result.Stdout))
	}
}

func TestLocalCommandRunner_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	runner := NewLocalCommandRunner(dir)

	_, err := runner.Exec(context.Background(), "sleep", []string{"10"}, ExecOptions{
		Timeout: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error when command is killed by timeout")
	}
}

func TestLocalCommandRunner_Stdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	runner := NewLocalCommandRunner(dir)

	result, err := runner.Exec(context.Background(), "cat", nil, ExecOptions{
		Stdin: []byte("hello from stdin"),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "hello from stdin" {
		t.Fatalf("Stdout = %q", result.Stdout)
	}
}

func TestLocalCommandRunner_MaxOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	runner := NewLocalCommandRunner(dir, WithMaxOutput(100))

	result, err := runner.Exec(context.Background(), "sh", []string{"-c", "yes | head -c 500"}, ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(result.Stdout) != 100 {
		t.Fatalf("len(Stdout) = %d, want 100", len(result.Stdout))
	}
}

func TestNoopCommandRunner(t *testing.T) {
	runner := NoopCommandRunner{}
	result, err := runner.Exec(context.Background(), "anything", nil, ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d", result.ExitCode)
	}
}

func TestLocalCommandRunner_WorkDirAbsoluteEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	runner := NewLocalCommandRunner(dir)

	_, err := runner.Exec(context.Background(), "pwd", nil, ExecOptions{
		WorkDir: "/tmp",
	})
	if err == nil {
		t.Fatal("absolute WorkDir outside rootDir should be rejected")
	}
	if !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("error should mention escape: %v", err)
	}
}

func TestLocalCommandRunner_WorkDirRelativeEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	runner := NewLocalCommandRunner(dir)

	_, err := runner.Exec(context.Background(), "pwd", nil, ExecOptions{
		WorkDir: "../../../tmp",
	})
	if err == nil {
		t.Fatal("relative WorkDir escaping rootDir should be rejected")
	}
}

func TestLocalCommandRunner_WorkDirSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	runner := NewLocalCommandRunner(root)
	_, err := runner.Exec(context.Background(), "pwd", nil, ExecOptions{
		WorkDir: filepath.Join(root, "link"),
	})
	if err == nil {
		t.Fatal("symlink WorkDir escaping rootDir should be rejected")
	}
}

func TestLocalCommandRunner_WorkDirValidSubdir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	runner := NewLocalCommandRunner(root)
	result, err := runner.Exec(context.Background(), "pwd", nil, ExecOptions{
		WorkDir: sub,
	})
	if err != nil {
		t.Fatalf("valid subdir should be allowed: %v", err)
	}
	if !strings.Contains(result.Stdout, "subdir") {
		t.Fatalf("expected pwd to show subdir, got %q", result.Stdout)
	}
}

func TestLocalCommandRunner_WorkDirRelativeValid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	runner := NewLocalCommandRunner(root)
	result, err := runner.Exec(context.Background(), "pwd", nil, ExecOptions{
		WorkDir: "child",
	})
	if err != nil {
		t.Fatalf("relative valid subdir should be allowed: %v", err)
	}
	if !strings.Contains(result.Stdout, "child") {
		t.Fatalf("expected pwd to show child, got %q", result.Stdout)
	}
}

func TestScopedCommandRunner_AllowedCommand(t *testing.T) {
	inner := NoopCommandRunner{}
	runner := NewScopedCommandRunner(inner, []string{"ls", "cat", "echo"})

	_, err := runner.Exec(context.Background(), "ls", nil, ExecOptions{})
	if err != nil {
		t.Fatalf("allowed command should succeed: %v", err)
	}
}

func TestScopedCommandRunner_BlockedCommand(t *testing.T) {
	inner := NoopCommandRunner{}
	runner := NewScopedCommandRunner(inner, []string{"ls", "cat"})

	_, err := runner.Exec(context.Background(), "rm", []string{"-rf", "/"}, ExecOptions{})
	if err == nil {
		t.Fatal("blocked command should fail")
	}
	if !strings.Contains(err.Error(), "whitelist") {
		t.Fatalf("error should mention whitelist: %v", err)
	}
}

func TestScopedCommandRunner_EmptyWhitelist(t *testing.T) {
	inner := NoopCommandRunner{}
	runner := NewScopedCommandRunner(inner, nil)

	_, err := runner.Exec(context.Background(), "ls", nil, ExecOptions{})
	if err == nil {
		t.Fatal("empty whitelist should block all commands")
	}
}
