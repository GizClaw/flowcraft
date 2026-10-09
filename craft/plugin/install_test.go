package plugin

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func manifestFor(id, version string) string {
	return `{
		"id": "` + id + `", "version": "` + version + `",
		"permissions": ["mcp:provide"],
		"mcp": {"command": "true"}
	}`
}

// newRootStore opens a store over one writable root.
func newRootStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestInstallUpdateRollback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	source := t.TempDir()
	writePlugin(t, source, "hello", manifestFor("hello", "0.1.0"))
	summary, err := store.Install(context.Background(),
		filepath.Join(source, "hello"))
	if err != nil || summary.Version != "0.1.0" {
		t.Fatalf("Install = %+v, %v", summary, err)
	}
	if _, err := store.Install(context.Background(),
		filepath.Join(source, "hello")); err == nil {
		t.Fatal("same-version install was accepted")
	}
	writePlugin(t, source, "hello", manifestFor("hello", "0.2.0"))
	summary, err = store.Install(context.Background(),
		filepath.Join(source, "hello"))
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("update = %+v, %v", summary, err)
	}
	if info, ok := store.Entry("hello"); !ok || info.Manifest.Version != "0.2.0" {
		t.Fatalf("entry after update = %+v, %v", info, ok)
	}
	writePlugin(t, source, "hello", manifestFor("hello", "0.1.0"))
	if _, err := store.Install(context.Background(),
		filepath.Join(source, "hello")); err == nil {
		t.Fatal("downgrade install was accepted")
	}
	rolled, err := store.Rollback(context.Background(), "hello")
	if err != nil || rolled.Version != "0.1.0" {
		t.Fatalf("Rollback = %+v, %v", rolled, err)
	}
	if info, ok := store.Entry("hello"); !ok || info.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after rollback = %+v, %v", info, ok)
	}
}

// builtinStore opens a store with one read-only root and one writable
// root, the shape an application ships plugins in.
func builtinStore(t *testing.T, builtin, user string) *Store {
	t.Helper()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: builtin, Builtin: true}, {Path: user}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// TestInstallOverBuiltinShadows pins the flow the scan documents: a
// builtin root is read-only, so updating a plugin the application ships
// means installing a user copy that shadows it. The copy must be
// strictly newer than the builtin, the builtin stays where it is, and
// the install keeps no snapshot: there is no copy of ours to restore.
func TestInstallOverBuiltinShadows(t *testing.T) {
	t.Parallel()
	builtin := t.TempDir()
	user := t.TempDir()
	writePlugin(t, builtin, "hello", manifestFor("hello", "0.5.0"))
	store := builtinStore(t, builtin, user)
	source := t.TempDir()

	// Older than the builtin it would hide: refused, and nothing is
	// written into the writable root.
	writePlugin(t, source, "hello", manifestFor("hello", "0.4.0"))
	if _, err := store.Install(context.Background(),
		filepath.Join(source, "hello")); err == nil {
		t.Fatal("an install older than the builtin was accepted")
	}
	if _, err := os.Stat(filepath.Join(user, "hello")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("refused install left something behind: %v", err)
	}

	writePlugin(t, source, "hello", manifestFor("hello", "0.6.0"))
	summary, err := store.Install(context.Background(), filepath.Join(source, "hello"))
	if err != nil || summary.Version != "0.6.0" {
		t.Fatalf("Install over builtin = %+v, %v", summary, err)
	}
	entry, ok := store.Entry("hello")
	if !ok || !entry.ShadowsBuiltin || entry.BuiltinVersion != "0.5.0" {
		t.Fatalf("entry after the shadow = %+v, %v", entry, ok)
	}
	if entry.Dir != filepath.Join(user, "hello") {
		t.Fatalf("shadow dir = %q, want the writable root", entry.Dir)
	}
	if info, err := os.Stat(filepath.Join(builtin, "hello", "plugin.json")); err != nil {
		t.Fatalf("builtin copy was touched: %v", err)
	} else if info.Size() == 0 {
		t.Fatal("builtin manifest was truncated")
	}

	// A shadow that fell behind a builtin the application updated since
	// may be newer than the copy and still older than what it hides.
	if err := os.WriteFile(filepath.Join(user, "hello", "plugin.json"),
		[]byte(manifestFor("hello", "0.3.0")), 0o600); err != nil {
		t.Fatalf("rewrite shadow: %v", err)
	}
	writePlugin(t, source, "hello", manifestFor("hello", "0.4.0"))
	_, err = store.Install(context.Background(), filepath.Join(source, "hello"))
	if err == nil || !strings.Contains(err.Error(), "older than builtin") {
		t.Fatalf("Install between shadow and builtin = %v, want a refusal", err)
	}
}

// TestInstallLandsInTheRootTheScanPrefers pins the two halves of the
// same rule: the scan lets a later root shadow an earlier one, so an
// install has to land in the last writable root. Writing into the first
// one would install a plugin the scan never looks at.
func TestInstallLandsInTheRootTheScanPrefers(t *testing.T) {
	t.Parallel()
	builtin := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	writePlugin(t, builtin, "hello", manifestFor("hello", "0.1.0"))
	store, err := NewStore(Options{
		Roots: []Root{
			{Path: builtin, Builtin: true},
			{Path: first},
			{Path: second},
		},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	source := t.TempDir()
	writePlugin(t, source, "hello", manifestFor("hello", "0.2.0"))
	if _, err := store.Install(context.Background(),
		filepath.Join(source, "hello")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Dir != filepath.Join(second, "hello") {
		t.Fatalf("entry = %+v, %v; want the shadowing root", entry, ok)
	}
	if _, err := os.Stat(filepath.Join(first, "hello")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("install landed in the root the scan ignores: %v", err)
	}
}
