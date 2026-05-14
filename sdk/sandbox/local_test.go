package sandbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestLocalRunner_Exec_HappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	runner := sandbox.NewLocalRunner(t.TempDir())

	result, err := runner.Exec(context.Background(), "echo", []string{"hello"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello") {
		t.Fatalf("Stdout = %q, want substring 'hello'", result.Stdout)
	}
}

func TestLocalRunner_Exec_NonZeroExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	runner := sandbox.NewLocalRunner(t.TempDir())

	result, err := runner.Exec(context.Background(), "sh", []string{"-c", "exit 42"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("non-zero exit should not return Go error, got: %v", err)
	}
	if result.ExitCode != 42 {
		t.Fatalf("ExitCode = %d, want 42", result.ExitCode)
	}
}

func TestLocalRunner_Exec_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	runner := sandbox.NewLocalRunner(t.TempDir())

	_, err := runner.Exec(context.Background(), "sleep", []string{"1"}, sandbox.ExecOptions{
		Timeout: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errdefs.IsTimeout(err) {
		t.Fatalf("expected errdefs.IsTimeout, got: %v", err)
	}
}

func TestLocalRunner_Exec_WorkDirEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	root := t.TempDir()
	runner := sandbox.NewLocalRunner(root)

	_, err := runner.Exec(context.Background(), "pwd", nil, sandbox.ExecOptions{
		WorkDir: "/tmp",
	})
	if err == nil {
		t.Fatal("absolute WorkDir outside root should be rejected")
	}
	if !errors.Is(err, sandbox.ErrPathTraversal) {
		t.Fatalf("expected sandbox.ErrPathTraversal, got: %v", err)
	}

	_, err = runner.Exec(context.Background(), "pwd", nil, sandbox.ExecOptions{
		WorkDir: "../../../tmp",
	})
	if err == nil {
		t.Fatal("relative WorkDir escaping root should be rejected")
	}
	if !errors.Is(err, sandbox.ErrPathTraversal) {
		t.Fatalf("expected sandbox.ErrPathTraversal, got: %v", err)
	}
}

func TestLocalRunner_MaxOutputBytes_Truncates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	runner := sandbox.NewLocalRunner(t.TempDir())

	t.Run("per-call override", func(t *testing.T) {
		result, err := runner.Exec(context.Background(), "sh", []string{"-c", "yes | head -c 500"}, sandbox.ExecOptions{
			Resources: sandbox.ResourceLimits{MaxOutputBytes: 100},
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if len(result.Stdout) != 100 {
			t.Fatalf("len(Stdout) = %d, want 100", len(result.Stdout))
		}
	})

	t.Run("runner default", func(t *testing.T) {
		r := sandbox.NewLocalRunner(t.TempDir(), sandbox.WithMaxOutputBytes(50))
		result, err := r.Exec(context.Background(), "sh", []string{"-c", "yes | head -c 500"}, sandbox.ExecOptions{})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if len(result.Stdout) != 50 {
			t.Fatalf("len(Stdout) = %d, want 50", len(result.Stdout))
		}
	})

	t.Run("per-call overrides default", func(t *testing.T) {
		r := sandbox.NewLocalRunner(t.TempDir(), sandbox.WithMaxOutputBytes(50))
		result, err := r.Exec(context.Background(), "sh", []string{"-c", "yes | head -c 500"}, sandbox.ExecOptions{
			Resources: sandbox.ResourceLimits{MaxOutputBytes: 200},
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if len(result.Stdout) != 200 {
			t.Fatalf("len(Stdout) = %d, want 200 (per-call override)", len(result.Stdout))
		}
	})
}

func TestLocalRunner_Env_NilAllow_InheritsAll(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	t.Setenv("SANDBOX_TEST_PARENT", "from-host")
	runner := sandbox.NewLocalRunner(t.TempDir())

	result, err := runner.Exec(context.Background(), "sh", []string{"-c", "echo $SANDBOX_TEST_PARENT"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "from-host" {
		t.Fatalf("nil Allow should inherit host env, got Stdout = %q", result.Stdout)
	}
}

func TestLocalRunner_Env_EmptyAllow_InheritsNone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	t.Setenv("SANDBOX_TEST_PARENT", "from-host")
	runner := sandbox.NewLocalRunner(t.TempDir())

	result, err := runner.Exec(context.Background(), "/bin/sh", []string{"-c", "echo \"<$SANDBOX_TEST_PARENT>\""}, sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{Allow: []string{}},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "<>" {
		t.Fatalf("empty Allow should hide host env, got Stdout = %q", result.Stdout)
	}
}

func TestLocalRunner_Env_Allow_Filters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	t.Setenv("SANDBOX_KEEP", "yes")
	t.Setenv("SANDBOX_DROP", "no")
	runner := sandbox.NewLocalRunner(t.TempDir())

	result, err := runner.Exec(context.Background(), "/bin/sh", []string{"-c", "echo \"keep=$SANDBOX_KEEP drop=$SANDBOX_DROP path_set=${PATH:+yes}\""}, sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{Allow: []string{"PATH", "SANDBOX_KEEP"}},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	out := strings.TrimSpace(result.Stdout)
	if !strings.Contains(out, "keep=yes") {
		t.Fatalf("SANDBOX_KEEP should be present, got: %q", out)
	}
	if !strings.Contains(out, "drop=") || strings.Contains(out, "drop=no") {
		t.Fatalf("SANDBOX_DROP should be filtered, got: %q", out)
	}
	if !strings.Contains(out, "path_set=yes") {
		t.Fatalf("PATH should be inherited via allow list, got: %q", out)
	}
}

func TestLocalRunner_Env_Inject_OverridesHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	t.Setenv("SANDBOX_FOO", "from-host")
	runner := sandbox.NewLocalRunner(t.TempDir())

	result, err := runner.Exec(context.Background(), "/bin/sh", []string{"-c", "echo $SANDBOX_FOO"}, sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{Inject: map[string]string{"SANDBOX_FOO": "from-inject"}},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "from-inject" {
		t.Fatalf("Inject should override host SANDBOX_FOO, got: %q", result.Stdout)
	}
}

func TestLocalRunner_Net_NonDefault_NotAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	runner := sandbox.NewLocalRunner(t.TempDir())

	_, err := runner.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{
		Net: sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
	})
	if err == nil {
		t.Fatal("expected NotAvailable error for NetDenyAll on LocalRunner")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected errdefs.IsNotAvailable, got: %v", err)
	}
}

func TestLocalRunner_Resources_NonZero_NotAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	runner := sandbox.NewLocalRunner(t.TempDir())

	for name, opts := range map[string]sandbox.ExecOptions{
		"cpu":  {Resources: sandbox.ResourceLimits{CPUMillicores: 100}},
		"mem":  {Resources: sandbox.ResourceLimits{MemoryBytes: 1 << 20}},
		"disk": {Resources: sandbox.ResourceLimits{DiskBytes: 1 << 20}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runner.Exec(context.Background(), "echo", nil, opts)
			if err == nil {
				t.Fatalf("%s limit should return NotAvailable on LocalRunner", name)
			}
			if !errdefs.IsNotAvailable(err) {
				t.Fatalf("%s: expected errdefs.IsNotAvailable, got: %v", name, err)
			}
		})
	}
}

func TestLocalRunner_WorkDirValidSubdir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "child")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := sandbox.NewLocalRunner(root)

	result, err := runner.Exec(context.Background(), "pwd", nil, sandbox.ExecOptions{WorkDir: "child"})
	if err != nil {
		t.Fatalf("valid subdir should be allowed: %v", err)
	}
	if !strings.Contains(result.Stdout, "child") {
		t.Fatalf("pwd should show child, got: %q", result.Stdout)
	}
}

func TestLocalRunner_WorkDirSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	runner := sandbox.NewLocalRunner(root)

	_, err := runner.Exec(context.Background(), "pwd", nil, sandbox.ExecOptions{
		WorkDir: filepath.Join(root, "link"),
	})
	if err == nil {
		t.Fatal("symlink escape should be rejected")
	}
	if !errors.Is(err, sandbox.ErrPathTraversal) {
		t.Fatalf("expected sandbox.ErrPathTraversal, got: %v", err)
	}
}

func TestLocalRunner_Stdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	runner := sandbox.NewLocalRunner(t.TempDir())

	result, err := runner.Exec(context.Background(), "cat", nil, sandbox.ExecOptions{
		Stdin: []byte("hello from stdin"),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "hello from stdin" {
		t.Fatalf("Stdout = %q", result.Stdout)
	}
}
