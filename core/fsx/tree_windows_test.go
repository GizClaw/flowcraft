//go:build windows

package fsx_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
)

// A target spelled with different case than its writable root must select the
// same handle. Windows filesystems fold case, so a case-sensitive selector
// would deny writes the handle itself allows.
func TestCaseInsensitiveRootSelection(t *testing.T) {
	root := t.TempDir()
	tree, ctx := openTree(t, root)

	target := filepath.Join(strings.ToLower(root), "sub", "a.txt")
	if err := tree.Write(ctx, target, []byte("x")); err != nil {
		t.Fatalf("Write through a differently-cased root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "a.txt")); err != nil {
		t.Errorf("write did not land under the root: %v", err)
	}
}

// One handle covers one volume. A target on another volume cannot be reached by
// any of them, so it must be refused rather than matched lexically.
func TestCrossVolumeTargetIsDenied(t *testing.T) {
	if _, err := os.Stat(`D:\`); err != nil {
		t.Skip("no second volume to test against")
	}

	root := t.TempDir()
	tree, ctx := openTree(t, root)

	err := tree.Write(ctx, `D:\fsx-cross-volume.txt`, []byte("x"))
	if !errors.Is(err, fsx.ErrDenied) {
		t.Errorf("write to another volume = %v, want fsx.ErrDenied", err)
	}
}

// Reserved device names are refused or treated as ordinary names inside the
// root; what they must never do is reach the device.
func TestReservedNameStaysInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	tree, ctx := openTree(t, root)

	target := filepath.Join(root, "NUL")
	_ = tree.Write(ctx, target, []byte("x"))

	outside, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range outside {
		if entry.Name() == "NUL" {
			t.Errorf("write to %q created an entry beside the root", target)
		}
	}
}
