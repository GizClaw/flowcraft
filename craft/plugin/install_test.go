package plugin

import (
	"context"
	"path/filepath"
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
