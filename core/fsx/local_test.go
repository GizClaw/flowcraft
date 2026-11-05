package fsx_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/fsx/fstest"
)

func newLocal(t *testing.T) (fsx.FS, string) {
	t.Helper()
	dir := t.TempDir()
	fs, err := fsx.NewLocal(dir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return fs, dir
}

func TestLocalContract(t *testing.T) { fstest.RunContract(t, newLocal) }

func TestNewLocalValidatesRoot(t *testing.T) {
	if _, err := fsx.NewLocal("relative/dir"); err == nil {
		t.Error("NewLocal(relative) = nil error, want a validation error")
	}
	if _, err := fsx.NewLocal(""); err == nil {
		t.Error("NewLocal(\"\") = nil error, want a validation error")
	}
}

func TestNewLocalCreatesMissingRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspace")
	if _, err := fsx.NewLocal(dir); err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Errorf("root was not created: err=%v info=%v", err, info)
	}
}

// A symlink planted inside the tree must not be usable to escape it: the
// lexical check passes, the symlink-resolved check does not.
func TestLocalRefusesSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	root := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fs, err := fsx.NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	if _, err := fs.Read(context.Background(), "escape/secret.txt", nil); !errors.Is(err, fsx.ErrDenied) {
		t.Errorf("Read through an escaping symlink = %v, want fsx.ErrDenied", err)
	}
}

// Write publishes through a temporary file plus rename, so no partial payload
// is ever observable and no temp file is left behind.
func TestLocalWriteLeavesNoTempFiles(t *testing.T) {
	root := t.TempDir()
	fs, err := fsx.NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	if err := fs.Write(ctx, "a.txt", []byte("first"), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := fs.Write(ctx, "a.txt", []byte("second"), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := fs.Read(ctx, "a.txt", nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".fsx-") {
			t.Errorf("temporary file %q was left behind", entry.Name())
		}
	}
}

// Reads are unrestricted inside the tree: no backend can enforce a read
// restriction, so fsx does not pretend to.
func TestLocalReadsAreUnrestrictedInsideTheTree(t *testing.T) {
	fs, _ := newLocal(t)
	ctx := context.Background()
	if err := fs.Write(ctx, "visible.txt", []byte("x"), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := fs.Read(ctx, "visible.txt", nil); err != nil {
		t.Errorf("Read inside the tree = %v, want nil", err)
	}
}
