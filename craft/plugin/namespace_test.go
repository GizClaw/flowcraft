package plugin

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestStoreMarksToolNamespaceCollisions covers the store half: two
// plugins whose ids collapse into one tool namespace — ToolPrefix
// rewrites "." — cannot both be published, and the one that lost the
// race is told which plugin it collides with instead of silently losing
// every tool it publishes.
//
// The collision is not an invalid manifest: the plugin is listed, it is
// readable, and it can be disabled — which is the recovery, because the
// collision is over as soon as one of the two stops claiming the
// namespace. Only enabling it is refused.
func TestStoreMarksToolNamespaceCollisions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "first", `{
		"id": "hello.world", "version": "0.1.0",
		"permissions": ["tools:provide"], "run": {"command": "true"}
	}`)
	writePlugin(t, root, "second", `{
		"id": "hello_world", "version": "0.2.0",
		"permissions": ["tools:provide"], "run": {"command": "true"}
	}`)
	store := newStoreOver(t, root)

	entries, err := store.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("valid plugins = %v, want both listed", entries)
	}
	collided, ok := store.Entry("hello_world")
	if !ok {
		t.Fatal("a plugin whose namespace is taken is not readable")
	}
	const want = `the tool namespace "hello_world__" collides with plugin "hello.world"`
	if collided.Conflict != want {
		t.Fatalf("conflict = %q, want %q", collided.Conflict, want)
	}
	if collided.Error != "" {
		t.Fatalf("error = %q, want a collision to be reported as a conflict", collided.Error)
	}
	// It is not started, and it cannot be enabled while the collision
	// lasts.
	enabled, err := store.Enabled()
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	for _, entry := range enabled {
		if entry.ID == "hello_world" {
			t.Fatal("a plugin whose namespace is taken was listed as startable")
		}
	}
	if err := store.SetEnabled("hello_world", true); !errdefs.IsConflict(err) {
		t.Fatalf("enabling the collided plugin = %v, want Conflict", err)
	}
	// Disabling it is the way out of the collision.
	if err := store.SetEnabled("hello_world", false); err != nil {
		t.Fatalf("disabling the collided plugin: %v", err)
	}
	if got, ok := store.Entry("hello_world"); !ok || got.Enabled {
		t.Fatalf("entry after the disable = %+v, %v", got, ok)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, summary := range list {
		if summary.ID != "hello_world" {
			continue
		}
		found = true
		if summary.Conflict != want {
			t.Fatalf("collision = %q, want %q", summary.Conflict, want)
		}
		if summary.Error != "" {
			t.Fatalf("error = %q, want the entry to be valid", summary.Error)
		}
	}
	if !found {
		t.Fatal("the colliding plugin is not listed at all")
	}
}
