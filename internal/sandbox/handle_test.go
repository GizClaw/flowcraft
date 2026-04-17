package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSandboxHandle_AcquireReleaseAndIdle(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.IdleTimeout = 100 * time.Millisecond
	if err := os.MkdirAll(filepath.Join(cfg.RootDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.RootDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	handle, err := NewSandboxHandle(context.Background(), "u1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	sb, done, err := handle.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if handle.UseCount() != 1 {
		t.Fatalf("expected useCount 1, got %d", handle.UseCount())
	}

	time.Sleep(150 * time.Millisecond)
	result, err := sb.Exec(context.Background(), "echo", []string{"still-open"}, ExecOptions{})
	if err != nil {
		t.Fatalf("sandbox should remain open while leased: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected successful exec, got exit %d", result.ExitCode)
	}

	done()
	if handle.UseCount() != 0 {
		t.Fatalf("expected useCount 0 after release, got %d", handle.UseCount())
	}

	time.Sleep(150 * time.Millisecond)
	_, err = sb.Exec(context.Background(), "echo", []string{"closed"}, ExecOptions{})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed after idle close, got %v", err)
	}
}

func TestSandboxHandle_MultipleLeases(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.IdleTimeout = time.Second
	if err := os.MkdirAll(filepath.Join(cfg.RootDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.RootDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	handle, err := NewSandboxHandle(context.Background(), "u2", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	sb1, done1, err := handle.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sb2, done2, err := handle.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sb1.ID() != sb2.ID() {
		t.Fatalf("expected same sandbox instance, got %q and %q", sb1.ID(), sb2.ID())
	}
	if handle.UseCount() != 2 {
		t.Fatalf("expected useCount 2, got %d", handle.UseCount())
	}

	done1()
	if handle.UseCount() != 1 {
		t.Fatalf("expected useCount 1 after first release, got %d", handle.UseCount())
	}
	done2()
	if handle.UseCount() != 0 {
		t.Fatalf("expected useCount 0 after second release, got %d", handle.UseCount())
	}
}
