package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
)

// --- SB-21: ExecTool.Execute 带有默认超时 ---

func TestExecTool_DefaultTimeout(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.ExecTimeout = 500 * time.Millisecond
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	tool := &ExecTool{Manager: m}
	ctx := model.WithRuntimeID(context.Background(), "timeout-test")

	start := time.Now()
	args := `{"command":"sleep 30"}`
	_, err = tool.Execute(ctx, args)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("command should have been killed by timeout, ran for %v", elapsed)
	}
	// sleep killed by context timeout produces an error
	if err == nil {
		t.Log("no error returned (may have non-zero exit instead)")
	}
}

func TestExecTool_CustomTimeout_ExceedsDefault(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.ExecTimeout = 500 * time.Millisecond
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	tool := &ExecTool{Manager: m}
	ctx := model.WithRuntimeID(context.Background(), "exceed-test")

	// LLM requests 3s which exceeds ExecTimeout (500ms); should be honored.
	args := `{"command":"sleep 1 && echo done","timeout":"3s"}`
	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("command should succeed with LLM-specified timeout: %v", err)
	}

	var res ExecResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "done") {
		t.Fatalf("unexpected output: %q", res.Stdout)
	}
}

func TestExecTool_CustomTimeout_Shorter(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.ExecTimeout = 10 * time.Second
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	tool := &ExecTool{Manager: m}
	ctx := model.WithRuntimeID(context.Background(), "short-test")

	args := `{"command":"echo ok","timeout":"500ms"}`
	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("short echo should succeed: %v", err)
	}

	var res ExecResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "ok") {
		t.Fatalf("unexpected output: %q", res.Stdout)
	}
}

func TestExecTool_NoRuntimeID(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	tool := &ExecTool{Manager: m}
	_, err = tool.Execute(context.Background(), `{"command":"echo"}`)
	if err == nil {
		t.Fatal("expected error for missing runtime ID")
	}
}

func TestExecTool_UsesSandboxHandleWithoutRuntimeID(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.ExecTimeout = 2 * time.Second
	if err := os.MkdirAll(filepath.Join(cfg.RootDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.RootDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	handle, err := NewSandboxHandle(context.Background(), "user-handle", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	tool := &ExecTool{}
	ctx := model.WithSandboxHandle(context.Background(), handle)
	ctx = model.WithRuntimeID(ctx, "user-handle")

	result, err := tool.Execute(ctx, `{"command":"echo handle-ok"}`)
	if err != nil {
		t.Fatalf("expected sandbox handle execution to succeed: %v", err)
	}

	var res ExecResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "handle-ok") {
		t.Fatalf("unexpected output: %q", res.Stdout)
	}
}
