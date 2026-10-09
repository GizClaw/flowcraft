//go:build !windows

package plugin

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestInstallFailureKeepsThePreviousState covers both ends of a failed
// copy, which a plugin tree carrying a symlink reaches without needing a
// broken filesystem: a fresh install leaves nothing behind, and a
// replace puts the snapshot back, with the enable state untouched.
func TestInstallFailureKeepsThePreviousState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newRootStore(t, root)
	source := t.TempDir()
	writePlugin(t, source, "hello", manifestFor("hello", "0.1.0"))
	link := filepath.Join(source, "hello", "link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	ctx := context.Background()

	if _, err := store.Install(ctx, filepath.Join(source, "hello")); !errdefs.IsForbidden(err) {
		t.Fatalf("install of a tree with a symlink = %v, want Forbidden", err)
	}
	if _, err := os.Stat(filepath.Join(root, "hello")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed fresh install left a tree behind: %v", err)
	}
	if entries, err := store.Entries(); err != nil || len(entries) != 0 {
		t.Fatalf("Entries after the failed install = %+v, %v", entries, err)
	}

	if err := os.Remove(link); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if _, err := store.Install(ctx, filepath.Join(source, "hello")); err != nil {
		t.Fatalf("install 0.1.0: %v", err)
	}
	if err := store.SetEnabled("hello", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	revision := store.Revision()

	writePlugin(t, source, "hello", manifestFor("hello", "0.2.0"))
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatalf("second symlink: %v", err)
	}
	if _, err := store.Install(ctx, filepath.Join(source, "hello")); !errdefs.IsForbidden(err) {
		t.Fatalf("update with a symlink = %v, want Forbidden", err)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after the failed update = %+v, %v", entry, ok)
	}
	if entry.Enabled {
		t.Fatal("failed update changed the enable state")
	}
	if store.Revision() != revision {
		t.Fatalf("revision moved to %d on a failed update", store.Revision())
	}
	if _, err := os.Stat(
		filepath.Join(root, "hello", "link")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused tree was copied anyway: %v", err)
	}
}
