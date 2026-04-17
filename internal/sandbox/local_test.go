package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalSandbox_ExecEcho(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-1", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	result, err := sb.Exec(context.Background(), "echo", []string{"hello"}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", result.ExitCode)
	}
	if result.Stdout != "hello\n" {
		t.Fatalf("expected 'hello\\n', got %q", result.Stdout)
	}
}

func TestLocalSandbox_ExecWithTimeout(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-2", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	start := time.Now()
	result, err := sb.Exec(context.Background(), "sleep", []string{"10"}, ExecOptions{
		Timeout: 200 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil && result != nil && result.ExitCode == 0 && elapsed > 5*time.Second {
		t.Fatal("expected command to be killed by timeout")
	}
	if elapsed > 3*time.Second {
		t.Fatal("timeout did not interrupt the command in time")
	}
}

func TestLocalSandbox_ExecWithTimeout_ChildProcess(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-child-timeout", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	// sh -c spawns a child sleep; both must be killed by timeout.
	start := time.Now()
	_, _ = sb.Exec(context.Background(), "sh", []string{"-c", "sleep 60 & sleep 60"}, ExecOptions{
		Timeout: 300 * time.Millisecond,
	})
	elapsed := time.Since(start)

	// Must return within WaitDelay (3s) + small margin, not hang for 60s.
	if elapsed > 5*time.Second {
		t.Fatalf("child process timeout: expected fast return, ran for %v", elapsed)
	}
}

func TestLocalSandbox_ReadWriteFile(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-3", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	ctx := context.Background()
	err = sb.WriteFile(ctx, "test.txt", []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}

	data, err := sb.ReadFile(ctx, "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("expected 'hello world', got %q", string(data))
	}
}

func TestLocalSandbox_ReadNotFound(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-4", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	_, err = sb.ReadFile(context.Background(), "nonexistent.txt")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLocalSandbox_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-5", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	_, err = sb.ReadFile(context.Background(), "../../etc/passwd")
	if err == nil {
		t.Fatal("expected path traversal error")
	}
}

func TestLocalSandbox_NestedDirs(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-6", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	ctx := context.Background()
	err = sb.WriteFile(ctx, "sub/dir/file.txt", []byte("nested"))
	if err != nil {
		t.Fatal(err)
	}

	data, err := sb.ReadFile(ctx, "sub/dir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "nested" {
		t.Fatalf("expected 'nested', got %q", string(data))
	}
}

func TestLocalSandbox_Closed(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-7", dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = sb.Exec(context.Background(), "echo", nil, ExecOptions{})
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}

	_, err = sb.ReadFile(context.Background(), "test.txt")
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestLocalSandbox_ID(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("my-id", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()
	if sb.ID() != "my-id" {
		t.Fatalf("expected 'my-id', got %q", sb.ID())
	}
}

func TestLocalSandbox_RootDir(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-8", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	root := sb.RootDir()
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("rootDir should exist: %v", err)
	}
}

func TestLocalSandbox_ExecEnv(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-9", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	result, err := sb.Exec(context.Background(), "sh", []string{"-c", "echo $MY_VAR"}, ExecOptions{
		Env: map[string]string{"MY_VAR": "test_value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "test_value\n" {
		t.Fatalf("expected 'test_value\\n', got %q", result.Stdout)
	}
}

func TestLocalSandbox_ExecWorkDir(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	_ = os.MkdirAll(subDir, 0o755)

	sb, err := NewLocalSandbox("test-10", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	result, err := sb.Exec(context.Background(), "pwd", nil, ExecOptions{
		WorkDir: "sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", result.ExitCode, result.Stderr)
	}
}

func TestLocalSandbox_ExecNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-11", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	result, err := sb.Exec(context.Background(), "sh", []string{"-c", "exit 42"}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 42 {
		t.Fatalf("expected exit 42, got %d", result.ExitCode)
	}
}

func TestLocalSandbox_ExecEcho_WithIsolation(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-isolation", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	result, err := sb.Exec(context.Background(), "echo", []string{"hello"}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", result.ExitCode)
	}
	if result.Stdout != "hello\n" {
		t.Fatalf("expected 'hello\\n', got %q", result.Stdout)
	}

	t.Logf("isolation backend: %s", sb.isolation.backend)
}

func TestLocalSandbox_IsolationBackendLogged(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-backend-log", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	backend := sb.isolation.backend.String()
	if backend != "bare" && backend != "bubblewrap" {
		t.Fatalf("unexpected backend: %s", backend)
	}
	t.Logf("detected backend: %s", backend)
}

func TestLocalSandbox_WithIsolationOption(t *testing.T) {
	dir := t.TempDir()
	injected := probeResult{backend: backendBare}
	sb, err := NewLocalSandbox("test-inject", dir, WithIsolation(injected))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	if sb.isolation.backend != backendBare {
		t.Fatalf("expected injected bare backend, got %s", sb.isolation.backend)
	}
}

func TestLocalSandbox_ExecTimeout_ProcessCleanup(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewLocalSandbox("test-cleanup", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	start := time.Now()
	_, _ = sb.Exec(context.Background(), "sleep", []string{"999"}, ExecOptions{
		Timeout: 200 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("process cleanup took too long: %v", elapsed)
	}
}

func TestLocalSandbox_ExecWithWorkDir_SubDir(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "mywork")
	_ = os.MkdirAll(subDir, 0o755)
	_ = os.WriteFile(filepath.Join(subDir, "marker.txt"), []byte("found"), 0o644)

	sb, err := NewLocalSandbox("test-workdir", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	result, err := sb.Exec(context.Background(), "cat", []string{"marker.txt"}, ExecOptions{
		WorkDir: "mywork",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", result.ExitCode, result.Stderr)
	}
	if result.Stdout != "found" {
		t.Fatalf("expected 'found', got %q", result.Stdout)
	}
}
