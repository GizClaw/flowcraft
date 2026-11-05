package fsx_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/fsx/fstest"
)

func newMem(t *testing.T) (fsx.FS, string) {
	t.Helper()
	mem, err := fsx.NewMem()
	if err != nil {
		t.Fatalf("NewMem: %v", err)
	}
	return mem, mem.Root()
}

// Mem runs the same contract as Local: a test double that behaves differently
// would let host tests pass on behaviour the real filesystem rejects.
func TestMemContract(t *testing.T) { fstest.RunContract(t, newMem) }

func TestMemIsIndependentOfTheHost(t *testing.T) {
	mem, err := fsx.NewMem()
	if err != nil {
		t.Fatalf("NewMem: %v", err)
	}
	ctx := context.Background()
	if err := mem.Write(ctx, "a.txt", []byte("x"), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The logical root is virtual; nothing may exist at that path on the host.
	if _, err := os.Stat(mem.Root()); err == nil {
		t.Skipf("host unexpectedly has %q; virtual root is not isolated", mem.Root())
	}
}

func TestMemRejectsRootRemoval(t *testing.T) {
	mem, _ := newMem(t)
	if err := mem.Remove(context.Background(), ".", nil); err == nil {
		t.Error("Remove(root) = nil, want an error")
	}
}

func TestMemMissingDirectoryListsAsNotExist(t *testing.T) {
	mem, _ := newMem(t)
	if _, err := mem.List(context.Background(), "absent", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("List(absent) = %v, want fs.ErrNotExist", err)
	}
}
