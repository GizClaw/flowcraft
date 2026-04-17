package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManager_AcquireRelease(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	sb, err := m.Acquire(context.Background(), "s1", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sb.ID() != "s1" {
		t.Fatalf("expected s1, got %q", sb.ID())
	}
	if m.Stats() != 1 {
		t.Fatalf("expected 1 sandbox, got %d", m.Stats())
	}

	if err := m.Release("s1"); err != nil {
		t.Fatal(err)
	}
}

func TestManager_SameSession(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	sb1, _ := m.Acquire(context.Background(), "s1", AcquireOptions{})
	sb2, _ := m.Acquire(context.Background(), "s1", AcquireOptions{})

	if sb1.ID() != sb2.ID() {
		t.Fatal("same session should return same sandbox")
	}
	if m.Stats() != 1 {
		t.Fatalf("expected 1, got %d", m.Stats())
	}

	_ = m.Release("s1") // refCount -> 1
	_ = m.Release("s1") // refCount -> 0
}

func TestManager_MaxConcurrent(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.MaxConcurrent = 2
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	_, _ = m.Acquire(context.Background(), "s1", AcquireOptions{})
	_, _ = m.Acquire(context.Background(), "s2", AcquireOptions{})

	_, err = m.Acquire(context.Background(), "s3", AcquireOptions{})
	if err == nil {
		t.Fatal("expected limit error")
	}
}

func TestManager_Ephemeral(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	_, err = m.Acquire(context.Background(), "s1", AcquireOptions{Mode: ModeEphemeral})
	if err != nil {
		t.Fatal(err)
	}

	_ = m.Release("s1")
	if m.Stats() != 0 {
		t.Fatalf("expected 0 (ephemeral destroyed), got %d", m.Stats())
	}
}

func TestManager_CloseIdempotent(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	_ = m.Close()
	_ = m.Close() // should not panic
}

func TestManager_CloseDestroysAll(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = m.Acquire(context.Background(), "s1", AcquireOptions{})
	_, _ = m.Acquire(context.Background(), "s2", AcquireOptions{})

	_ = m.Close()
	if m.Stats() != 0 {
		t.Fatalf("expected 0 after close, got %d", m.Stats())
	}
}

func TestManager_CircuitBreaker_Success(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	// Circuit breaker should be closed (passing through) by default
	sb, err := m.Acquire(context.Background(), "cb-1", AcquireOptions{})
	if err != nil {
		t.Fatalf("expected success through circuit breaker: %v", err)
	}
	if sb == nil {
		t.Fatal("expected sandbox")
	}
}

func TestManagerConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ManagerConfig
		wantErr bool
	}{
		{"negative_exec_timeout", ManagerConfig{ExecTimeout: -1}, true},
		{"negative_idle_timeout", ManagerConfig{IdleTimeout: -1}, true},
		{"negative_max_concurrent", ManagerConfig{MaxConcurrent: -1}, true},
		{"zero_values_ok", ManagerConfig{}, false},
		{"valid", ManagerConfig{ExecTimeout: time.Minute, IdleTimeout: time.Minute, MaxConcurrent: 10}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

func TestParseMode(t *testing.T) {
	if ParseMode("ephemeral") != ModeEphemeral {
		t.Fatal("expected ephemeral")
	}
	if ParseMode("session") != ModeSession {
		t.Fatal("expected session")
	}
	if ParseMode("persistent") != ModePersistent {
		t.Fatal("expected persistent")
	}
	if ParseMode("unknown") != ModeSession {
		t.Fatal("expected session as default")
	}
}

// --- SB-22: Manager local 分支创建的 sandbox 包含 skills/ 和 data/ 软链接 ---

func TestManager_LocalBranch_CreatesSymlinks(t *testing.T) {
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills")
	dataDir := filepath.Join(workspaceRoot, "data")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultManagerConfig()
	cfg.RootDir = workspaceRoot
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	sb, err := m.Acquire(context.Background(), "sym-test", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = sb

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sym-test")

	// Verify skills symlink
	skillsLink := filepath.Join(sandboxRoot, "skills")
	target, err := os.Readlink(skillsLink)
	if err != nil {
		t.Fatalf("skills symlink should exist: %v", err)
	}
	if target != skillsDir {
		t.Fatalf("skills symlink target = %q, want %q", target, skillsDir)
	}

	// Verify data symlink
	dataLink := filepath.Join(sandboxRoot, "data")
	target, err = os.Readlink(dataLink)
	if err != nil {
		t.Fatalf("data symlink should exist: %v", err)
	}
	if target != dataDir {
		t.Fatalf("data symlink target = %q, want %q", target, dataDir)
	}
}

func TestManager_LocalBranch_SkillReadable(t *testing.T) {
	workspaceRoot := t.TempDir()
	skillDir := filepath.Join(workspaceRoot, "skills", "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "main.py"), []byte("print('hello')"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultManagerConfig()
	cfg.RootDir = workspaceRoot
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	sb, err := m.Acquire(context.Background(), "read-test", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}

	data, err := sb.ReadFile(context.Background(), "skills/demo/main.py")
	if err != nil {
		t.Fatalf("should be able to read skill file through symlink: %v", err)
	}
	if string(data) != "print('hello')" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

func TestManager_LocalIsolation_CachedAcrossSandboxes(t *testing.T) {
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultManagerConfig()
	cfg.RootDir = workspaceRoot
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	sb1, err := m.Acquire(context.Background(), "cache-1", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sb2, err := m.Acquire(context.Background(), "cache-2", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}

	local1 := sb1.(*LocalSandbox)
	local2 := sb2.(*LocalSandbox)

	if local1.isolation.backend != local2.isolation.backend {
		t.Fatalf("expected same isolation backend, got %s and %s",
			local1.isolation.backend, local2.isolation.backend)
	}
	if local1.isolation.bwrapPath != local2.isolation.bwrapPath {
		t.Fatalf("expected same bwrapPath, got %q and %q",
			local1.isolation.bwrapPath, local2.isolation.bwrapPath)
	}

	expectedBackend := m.localIsolation.backend
	if local1.isolation.backend != expectedBackend {
		t.Fatalf("sandbox backend %s != manager cached backend %s",
			local1.isolation.backend, expectedBackend)
	}
}

func TestManager_Config(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.RootDir = t.TempDir()
	cfg.NetworkMode = "bridge"
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	got := m.Config()
	if got.NetworkMode != "bridge" {
		t.Fatalf("Config().NetworkMode = %q, want %q", got.NetworkMode, "bridge")
	}
	if got.ExecTimeout != cfg.ExecTimeout {
		t.Fatalf("Config().ExecTimeout = %v, want %v", got.ExecTimeout, cfg.ExecTimeout)
	}
}
