package plugin

import (
	"testing"
)

// TestHostListShowsWhatEntriesHides pins the two views apart: Entries is
// the enumeration a shell renders — valid plugins, with the revision the
// read started at — and List is the management view, where a plugin that
// failed to load still shows up, carrying the error that kept it out of
// Entries.
func TestHostListShowsWhatEntriesHides(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	writePlugin(t, root, "bad", `{
		"id": "bad", "version": "0.1.0", "permissions": ["nope"]
	}`)
	host := newAssetHost(t, root)

	entries, err := host.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "hello" {
		t.Fatalf("Entries = %+v, want hello only", entries)
	}
	list, err := host.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %+v, want the invalid plugin too", list)
	}
	found := false
	for _, summary := range list {
		if summary.ID != "bad" {
			continue
		}
		found = true
		if summary.Error == "" {
			t.Fatal("List reported the invalid plugin without its error")
		}
	}
	if !found {
		t.Fatalf("List = %+v, want bad among them", list)
	}
}

// TestHostKVDeleteAndList pins the halves of the plugin key/value store
// a plugin drives itself: List hands out a copy it cannot write back
// through, and Delete persists — a reopened store does not find the key
// again.
func TestHostKVDeleteAndList(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := t.TempDir()
	data := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	open := func() *Host {
		t.Helper()
		store, err := NewStore(Options{
			Roots:       []Root{{Path: root}},
			StateDir:    state,
			DataDirRoot: data,
		})
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		host, err := NewHost(HostOptions{Store: store})
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		return host
	}
	kv, err := open().KV("hello")
	if err != nil {
		t.Fatalf("KV: %v", err)
	}
	if err := kv.Set("a", "1"); err != nil {
		t.Fatalf("Set(a): %v", err)
	}
	if err := kv.Set("b", "2"); err != nil {
		t.Fatalf("Set(b): %v", err)
	}
	list := kv.List()
	if len(list) != 2 || list["a"] != "1" || list["b"] != "2" {
		t.Fatalf("List = %v, want both keys", list)
	}
	delete(list, "a")
	if _, ok := kv.Get("a"); !ok {
		t.Fatal("List handed out the store's own map")
	}
	if err := kv.Delete("a"); err != nil {
		t.Fatalf("Delete(a): %v", err)
	}
	if _, ok := kv.Get("a"); ok {
		t.Fatal("a survived Delete")
	}

	reopened, err := open().KV("hello")
	if err != nil {
		t.Fatalf("KV after reopen: %v", err)
	}
	if _, ok := reopened.Get("a"); ok {
		t.Fatal("the deleted key came back with the store")
	}
	if value, ok := reopened.Get("b"); !ok || value != "2" {
		t.Fatalf("b after reopen = %q/%v, want 2", value, ok)
	}
	if got := reopened.List(); len(got) != 1 {
		t.Fatalf("List after reopen = %v, want one key", got)
	}
}
