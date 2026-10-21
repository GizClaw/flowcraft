package plugin

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// eventually polls a condition until it holds or the bound runs out. The
// reconciliation pass runs on its own goroutine behind a revision
// signal, so a test waits for its effect rather than assuming it already
// happened.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle waits long enough for a reconciliation pass that should not
// happen to have happened anyway. It bounds the "nothing changed" half
// of the tests below, which is otherwise unfalsifiable.
func settle() { time.Sleep(150 * time.Millisecond) }

// TestHostStopsAPluginTheRecordNoLongerWants covers the safety net for
// a store change the host did not make: an application that removes a
// plugin through the store leaves its process running, and the store has
// no notion of processes (that is the host's). The pass that follows the
// revision takes the process away and reports the stop, so the record
// and the process table agree again.
func TestHostStopsAPluginTheRecordNoLongerWants(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	host, store := startVersionHost(t, factory)
	if !running(host, "hello") {
		t.Fatal("the plugin did not start")
	}
	if err := store.Uninstall(context.Background(), "hello",
		UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall through the store: %v", err)
	}
	eventually(t, "the process of the removed plugin to be stopped", func() bool {
		return !running(host, "hello")
	})
	if names := toolNames(host); len(names) != 0 {
		t.Fatalf("tools after the removal = %v, want none", names)
	}
	if got := factory.closedVersions(); !slices.Equal(got, []string{"0.1.0"}) {
		t.Fatalf("closed versions = %v, want the removed plugin's process", got)
	}
}

// TestHostRestartsAPluginTheRecordChanged covers the other direction:
// the bytes under a running plugin were replaced by something other than
// the host's own install (an application talking to the store, a file
// put there by hand). The process is restarted from what the record now
// names, because the record is what the application reads.
func TestHostRestartsAPluginTheRecordChanged(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	host, store := startVersionHost(t, factory)
	source := t.TempDir()
	writePlugin(t, source, "hello", manifestFor("hello", "0.2.0"))
	if _, err := store.Install(context.Background(),
		filepath.Join(source, "hello")); err != nil {
		t.Fatalf("Install through the store: %v", err)
	}
	eventually(t, "the new version to be started", func() bool {
		return slices.Equal(factory.versions(), []string{"0.1.0", "0.2.0"})
	})
	if entry, ok := store.Entry("hello"); !ok || entry.Manifest.Version != "0.2.0" {
		t.Fatalf("entry = %+v, %v", entry, ok)
	}
	if !running(host, "hello") {
		t.Fatal("the restarted plugin has no process")
	}
	if names := toolNames(host); !slices.Contains(names, "hello__echo") {
		t.Fatalf("tools after the restart = %v, want the plugin's", names)
	}
}

// TestHostLeavesAPluginInStepAlone pins the idempotence the pass relies
// on: the host's own operations end with the process running what the
// record names, so the revision they bump does not restart anything a
// second time.
func TestHostLeavesAPluginInStepAlone(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	host, _ := startVersionHost(t, factory)
	if _, err := installVersion(t, host, "0.2.0"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	settle()
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0", "0.2.0"}) {
		t.Fatalf("started versions = %v, want exactly one restart for the install", got)
	}
	// An enable of a plugin that is already running starts nothing, and
	// the pass that follows the revision agrees with it.
	if err := host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	settle()
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0", "0.2.0"}) {
		t.Fatalf("started versions after the enable = %v, want no extra start", got)
	}
}

// TestHostRefusesToStartAPluginWhoseNamespaceIsTaken covers the plugin
// the store lists but will not publish: enabling it is refused before
// any process exists, and disabling it — the way out of the collision —
// still works.
func TestHostRefusesToStartAPluginWhoseNamespaceIsTaken(t *testing.T) {
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
	if err := store.SetEnabled("hello_world", false); err != nil {
		t.Fatalf("disable the colliding plugin: %v", err)
	}
	factory := newVersionFactory()
	host := startHost(t, store, factory.new)

	ctx := context.Background()
	if err := host.SetEnabled(ctx, "hello_world", true); !errdefs.IsConflict(err) {
		t.Fatalf("enabling a plugin whose namespace is taken = %v, want Conflict", err)
	}
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0"}) {
		t.Fatalf("started versions = %v, want nothing started for the collision", got)
	}
	if err := host.SetEnabled(ctx, "hello_world", false); err != nil {
		t.Fatalf("disabling the colliding plugin: %v", err)
	}
	if entry, ok := store.Entry("hello_world"); !ok || entry.Enabled {
		t.Fatalf("entry after the disable = %+v, %v", entry, ok)
	}
}
