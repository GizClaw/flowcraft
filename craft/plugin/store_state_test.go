package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// stateStore opens a store over two plugins and returns it with the path
// of its enable state, so a test can damage the file the way a crash
// mid-write would.
func stateStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	stateDir := t.TempDir()
	for _, id := range []string{"alpha", "beta"} {
		writePlugin(t, root, id, `{
			"id": "`+id+`", "version": "0.1.0",
			"permissions": ["mcp:provide"], "mcp": {"command": "true"}
		}`)
	}
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    stateDir,
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, filepath.Join(stateDir, "enabled.json")
}

func readState(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	state := map[string]bool{}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	return state
}

// TestStoreWithoutStateFileEnablesByDefault pins the fresh-install case:
// no recorded state is not the same as a broken one.
func TestStoreWithoutStateFileEnablesByDefault(t *testing.T) {
	t.Parallel()
	store, _ := stateStore(t)
	entries, err := store.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want both plugins", entries)
	}
	for _, entry := range entries {
		if !entry.Enabled {
			t.Fatalf("%s reads as disabled without any recorded state", entry.ID)
		}
	}
}

// TestStoreRecordsDisabledPlugins is the round trip a user's toggle
// takes: the write lands in the state file, and the next scan reads it
// back.
func TestStoreRecordsDisabledPlugins(t *testing.T) {
	t.Parallel()
	store, path := stateStore(t)
	if err := store.SetEnabled("beta", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if state := readState(t, path); state["beta"] {
		t.Fatalf("recorded state = %v, want beta disabled", state)
	}
	entry, ok := store.Entry("beta")
	if !ok || entry.Enabled {
		t.Fatalf("entry after the disable = %+v, %v", entry, ok)
	}
	enabled, err := store.Enabled()
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	if len(enabled) != 1 || enabled[0].ID != "alpha" {
		t.Fatalf("enabled plugins = %v, want alpha only", enabled)
	}
}

// TestStoreFailsClosedOnAnUnreadableState pins what a state file that
// cannot be read means. Reading it as "no records" would silently
// re-enable every plugin a user had switched off, so the failure is
// reported, the plugins read as disabled, and the next write keeps them
// that way instead of handing them back.
func TestStoreFailsClosedOnAnUnreadableState(t *testing.T) {
	t.Parallel()
	store, path := stateStore(t)
	if err := os.WriteFile(path, []byte(`{"alpha": false,`), 0o600); err != nil {
		t.Fatalf("damage state: %v", err)
	}

	if _, err := store.Entries(); err == nil {
		t.Fatal("Entries accepted an unreadable state file")
	}
	if _, err := store.Enabled(); err == nil {
		t.Fatal("Enabled accepted an unreadable state file")
	}
	list, err := store.List()
	if err == nil {
		t.Fatal("List did not report the unreadable state file")
	}
	if len(list) != 2 {
		t.Fatalf("List = %v, want both plugins listed with the error", list)
	}
	for _, summary := range list {
		if summary.Enabled {
			t.Fatalf("%s reads as enabled under an unreadable state file",
				summary.ID)
		}
	}

	// The write starts from the fail-closed view, so the plugin the
	// caller did not touch stays off even though the file that said so
	// was unreadable.
	if err := store.SetEnabled("alpha", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	want := map[string]bool{"alpha": true, "beta": false}
	if state := readState(t, path); !reflect.DeepEqual(state, want) {
		t.Fatalf("state after the write = %v, want %v", state, want)
	}
}
