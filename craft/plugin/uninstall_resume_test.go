package plugin

import (
	"context"
	"os"
	"slices"
	"testing"
)

// TestUninstallResumesAPluginTheStoreRefusedToRemove pins the half of the
// uninstall's order that is not the lock: the store change is the only
// step that takes a plugin away, so a store that refuses must leave a
// plugin that was running running again. The failure here is the store's
// own — the plugin's directory cannot be removed, so Store.Uninstall
// refuses before anything is deleted — which is the case the resume
// exists for, and the one where the record has to survive whole: still
// installed, still enabled, and served by a process again.
//
// Holding a directory read-only is what makes the removal fail, which
// needs a user for whom permission bits are a restriction; under root
// the test has nothing to hold it back and is skipped.
func TestUninstallResumesAPluginTheStoreRefusedToRemove(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("holding a directory read-only needs a non-root user")
	}
	factory := newVersionFactory()
	host, store := startVersionHost(t, factory)
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0"}) {
		t.Fatalf("started versions = %v, want the installed one", got)
	}
	entry, ok := store.Entry("hello")
	if !ok {
		t.Fatal("the plugin is not installed to begin with")
	}
	if err := os.Chmod(entry.Dir, 0o555); err != nil {
		t.Fatalf("making the plugin directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(entry.Dir, 0o755) })

	if err := host.Uninstall(context.Background(), "hello", UninstallOptions{}); err == nil {
		t.Fatal("the uninstall reported success over a plugin it could not remove")
	}

	after, ok := store.Entry("hello")
	if !ok || !after.Enabled || after.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after the refused uninstall = %+v, %v, want it still installed and enabled",
			after, ok)
	}
	if got := factory.closedVersions(); !slices.Equal(got, []string{"0.1.0"}) {
		t.Fatalf("closed versions = %v, want the process the stop took out of the way", got)
	}
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0", "0.1.0"}) {
		t.Fatalf("started versions = %v, want the refused plugin's process back up", got)
	}
	if !running(host, "hello") {
		t.Fatal("the host has no process after the refused uninstall")
	}
	if names := toolNames(host); !slices.Contains(names, "hello__echo") {
		t.Fatalf("tools after the refused uninstall = %v, want the plugin's published again", names)
	}
}
