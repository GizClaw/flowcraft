package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// --- SB-14: WithSymlinks 创建软链接后 ReadFile/Exec 可达 ---

func TestWithSymlinks_ReadFile(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills", "demo")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "main.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sess1")
	sb, err := NewLocalSandbox("sess1", sandboxRoot, WithSymlinks([]SymlinkSpec{
		{Name: "skills", Target: filepath.Join(workspaceRoot, "skills"), ReadOnly: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	data, err := sb.ReadFile(context.Background(), "skills/demo/main.py")
	if err != nil {
		t.Fatalf("ReadFile through symlink should succeed: %v", err)
	}
	if string(data) != "print('hi')" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

func TestWithSymlinks_Exec(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills", "demo")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sess2")
	sb, err := NewLocalSandbox("sess2", sandboxRoot, WithSymlinks([]SymlinkSpec{
		{Name: "skills", Target: filepath.Join(workspaceRoot, "skills"), ReadOnly: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	result, err := sb.Exec(context.Background(), "ls", nil, ExecOptions{
		WorkDir: "skills/demo",
	})
	if err != nil {
		t.Fatalf("Exec with symlinked WorkDir should succeed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", result.ExitCode, result.Stderr)
	}
}

func TestWithSymlinks_WriteFile(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	dataDir := filepath.Join(workspaceRoot, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sess3")
	sb, err := NewLocalSandbox("sess3", sandboxRoot, WithSymlinks([]SymlinkSpec{
		{Name: "data", Target: dataDir, ReadOnly: false},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	ctx := context.Background()
	if err := sb.WriteFile(ctx, "data/output.csv", []byte("a,b,c")); err != nil {
		t.Fatalf("WriteFile through symlink should succeed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dataDir, "output.csv"))
	if err != nil {
		t.Fatalf("file should exist at real target: %v", err)
	}
	if string(content) != "a,b,c" {
		t.Fatalf("unexpected content: %q", string(content))
	}
}

func TestWithSymlinks_Idempotent(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sess4")
	specs := []SymlinkSpec{{Name: "skills", Target: skillsDir, ReadOnly: true}}

	sb1, err := NewLocalSandbox("sess4", sandboxRoot, WithSymlinks(specs))
	if err != nil {
		t.Fatal(err)
	}
	if err := sb1.Close(); err != nil {
		t.Fatal(err)
	}

	sb2, err := NewLocalSandbox("sess4", sandboxRoot, WithSymlinks(specs))
	if err != nil {
		t.Fatalf("second creation with same symlinks should succeed: %v", err)
	}
	defer func() { _ = sb2.Close() }()

	target, err := os.Readlink(filepath.Join(sandboxRoot, "skills"))
	if err != nil {
		t.Fatalf("symlink should exist: %v", err)
	}
	if target != skillsDir {
		t.Fatalf("symlink target mismatch: got %q, want %q", target, skillsDir)
	}
}

// --- SB-15: resolvePath 白名单路径放行 ---

func TestResolvePath_AllowedSymlink(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills")
	if err := os.MkdirAll(filepath.Join(skillsDir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sess5")
	sb, err := NewLocalSandbox("sess5", sandboxRoot, WithSymlinks([]SymlinkSpec{
		{Name: "skills", Target: skillsDir, ReadOnly: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	resolved, err := sb.resolvePath("skills/demo")
	if err != nil {
		t.Fatalf("resolvePath should allow whitelisted symlink: %v", err)
	}
	expected := filepath.Join(sb.rootDir, "skills", "demo")
	if resolved != expected {
		t.Fatalf("resolved path mismatch: got %q, want %q", resolved, expected)
	}
}

func TestResolvePath_AllowedSymlink_NestedFile(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills")
	subDir := filepath.Join(skillsDir, "analysis")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "run.sh"), []byte("#!/bin/sh"), 0o644); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sess6")
	sb, err := NewLocalSandbox("sess6", sandboxRoot, WithSymlinks([]SymlinkSpec{
		{Name: "skills", Target: skillsDir, ReadOnly: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	_, err = sb.resolvePath("skills/analysis/run.sh")
	if err != nil {
		t.Fatalf("resolvePath should allow nested files under whitelisted symlink: %v", err)
	}
}

func TestResolvePath_LocalFilesStillWork(t *testing.T) {
	skipIfNotLinux(t)
	sandboxRoot := t.TempDir()
	sb, err := NewLocalSandbox("sess7", sandboxRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	if err := os.WriteFile(filepath.Join(sandboxRoot, "local.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = sb.resolvePath("local.txt")
	if err != nil {
		t.Fatalf("resolvePath should still allow local files: %v", err)
	}
}

// --- SB-16: resolvePath 非白名单 symlink escape 拒绝 ---

func TestResolvePath_UnauthorizedSymlink_Rejected(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	secretDir := filepath.Join(workspaceRoot, "secrets")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "key.pem"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sess8")
	if err := os.MkdirAll(sandboxRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// Manually create an unauthorized symlink (not via WithSymlinks)
	if err := os.Symlink(secretDir, filepath.Join(sandboxRoot, "secrets")); err != nil {
		t.Fatal(err)
	}

	sb, err := NewLocalSandbox("sess8", sandboxRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	_, err = sb.resolvePath("secrets/key.pem")
	if err == nil {
		t.Fatal("resolvePath should reject unauthorized symlink escape")
	}
	if !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("expected ErrPathTraversal, got: %v", err)
	}
}

func TestResolvePath_PathTraversal_StillRejected(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "sess9")
	sb, err := NewLocalSandbox("sess9", sandboxRoot, WithSymlinks([]SymlinkSpec{
		{Name: "skills", Target: skillsDir, ReadOnly: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	_, err = sb.resolvePath("../../etc/passwd")
	if err == nil {
		t.Fatal("resolvePath should still reject path traversal even with allowed symlinks")
	}
	if !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("expected ErrPathTraversal, got: %v", err)
	}
}

// --- SymlinkSpec ReadOnly flag verification ---

func TestSymlinkSpec_ReadOnlyTargetClassification(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills")
	dataDir := filepath.Join(workspaceRoot, "data")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sandboxRoot := filepath.Join(workspaceRoot, "local", "ro-test")
	sb, err := NewLocalSandbox("ro-test", sandboxRoot, WithSymlinks([]SymlinkSpec{
		{Name: "skills", Target: skillsDir, ReadOnly: true},
		{Name: "data", Target: dataDir, ReadOnly: false},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	if len(sb.readOnlyTargets) != 1 {
		t.Fatalf("expected 1 readOnlyTarget, got %d: %v", len(sb.readOnlyTargets), sb.readOnlyTargets)
	}
	if len(sb.readWriteTargets) != 1 {
		t.Fatalf("expected 1 readWriteTarget, got %d: %v", len(sb.readWriteTargets), sb.readWriteTargets)
	}

	roResolved, _ := filepath.EvalSymlinks(skillsDir)
	if roResolved == "" {
		roResolved = skillsDir
	}
	if sb.readOnlyTargets[0] != roResolved {
		t.Fatalf("readOnlyTargets[0] = %q, want %q", sb.readOnlyTargets[0], roResolved)
	}

	rwResolved, _ := filepath.EvalSymlinks(dataDir)
	if rwResolved == "" {
		rwResolved = dataDir
	}
	if sb.readWriteTargets[0] != rwResolved {
		t.Fatalf("readWriteTargets[0] = %q, want %q", sb.readWriteTargets[0], rwResolved)
	}
}

func TestSymlinkSpec_AllTargetsContainsBoth(t *testing.T) {
	skipIfNotLinux(t)
	workspaceRoot := t.TempDir()
	skillsDir := filepath.Join(workspaceRoot, "skills")
	dataDir := filepath.Join(workspaceRoot, "data")
	_ = os.MkdirAll(skillsDir, 0o755)
	_ = os.MkdirAll(dataDir, 0o755)

	sandboxRoot := filepath.Join(workspaceRoot, "local", "all-test")
	sb, err := NewLocalSandbox("all-test", sandboxRoot, WithSymlinks([]SymlinkSpec{
		{Name: "skills", Target: skillsDir, ReadOnly: true},
		{Name: "data", Target: dataDir, ReadOnly: false},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	all := sb.allTargets()
	if len(all) != 2 {
		t.Fatalf("expected 2 allTargets, got %d", len(all))
	}

	_, err = sb.resolvePath("skills")
	if err != nil {
		t.Fatalf("resolvePath should allow skills (readOnly): %v", err)
	}
	_, err = sb.resolvePath("data")
	if err != nil {
		t.Fatalf("resolvePath should allow data (readWrite): %v", err)
	}
}
