//go:build windows

package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestRenameDirOverExistingRejected verifies the Windows-only guard:
// replacing an existing target with a directory is rejected with a
// clear contract-level error instead of a platform-specific one.
func TestRenameDirOverExistingRejected(t *testing.T) {
	ws, ctx := newLocalWS(t)
	for _, dir := range []string{"src", "dst"} {
		if err := os.MkdirAll(filepath.Join(ws.Root(), dir), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	if err := ws.Rename(ctx, "src", "dst"); err == nil {
		t.Fatal("Rename succeeded, want validation error")
	} else if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want validation error", err)
	}
}

// TestRenameDirToNewNameAllowed verifies moving a directory to a
// non-existing destination still works on Windows; only replacement
// of an existing target is rejected.
func TestRenameDirToNewNameAllowed(t *testing.T) {
	ws, ctx := newLocalWS(t)
	if err := os.MkdirAll(filepath.Join(ws.Root(), "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll src: %v", err)
	}
	if err := ws.Rename(ctx, "src", "moved"); err != nil {
		t.Fatalf("Rename dir to new name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "src")); !os.IsNotExist(err) {
		t.Fatalf("src still exists after rename: %v", err)
	}
	if info, err := os.Stat(filepath.Join(ws.Root(), "moved")); err != nil || !info.IsDir() {
		t.Fatalf("moved dir missing or not a dir: info=%v err=%v", info, err)
	}
}

// TestRenameFileOverExistingDirRejected covers the file-over-directory
// shape, which the guard also rejects with a clear error.
func TestRenameFileOverExistingDirRejected(t *testing.T) {
	ws, ctx := newLocalWS(t)
	mustWrite(t, ws, "file.txt", []byte("x"))
	if err := os.MkdirAll(filepath.Join(ws.Root(), "dst"), 0o755); err != nil {
		t.Fatalf("MkdirAll dst: %v", err)
	}
	if err := ws.Rename(ctx, "file.txt", "dst"); err == nil {
		t.Fatal("Rename succeeded, want validation error")
	} else if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want validation error", err)
	}
}
