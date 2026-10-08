package plugin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// contributionManifest gated declares every gated contribution; the
// permissions list is the variable under test.
func contributionManifest(id string, permissions ...string) string {
	granted := ""
	for i, permission := range permissions {
		if i > 0 {
			granted += ", "
		}
		granted += `"` + permission + `"`
	}
	return `{
		"id": "` + id + `", "version": "0.1.0",
		"permissions": [` + granted + `],
		"skills": ["skills"],
		"hooks": ["hooks/on_start.json"],
		"nodes": [{"type": "echo", "tool": "node_echo"}],
		"mcp": {"command": "python3", "args": ["server.py"]}
	}`
}

// writeContributionTree lays out the skill and hook files a manifest
// declares.
func writeContributionTree(t *testing.T, root, dir string) {
	t.Helper()
	for _, rel := range []string{
		filepath.Join("skills", "echo", "SKILL.md"),
		filepath.Join("hooks", "on_start.json"),
	} {
		path := filepath.Join(root, dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// TestContributionsRequirePermission covers the fail-closed half of the
// grant table: a plugin that declares a contribution without the
// matching permission still loads, but contributes nothing.
func TestContributionsRequirePermission(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const grantedID = "granted"
	const ungrantedID = "ungranted"
	writePlugin(t, root, grantedID, contributionManifest(
		grantedID, "skills:provide", "hooks:provide", "nodes:provide", "mcp:provide"))
	writePlugin(t, root, ungrantedID, contributionManifest(ungrantedID))
	for _, dir := range []string{grantedID, ungrantedID} {
		writeContributionTree(t, root, dir)
	}
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := NewHost(HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}

	// Both plugins install and enable: the gate is on contributions,
	// not on the plugin itself.
	enabled, err := store.Enabled()
	if err != nil || len(enabled) != 2 {
		t.Fatalf("Enabled = %d entries, %v; want both plugins", len(enabled), err)
	}

	skills := host.SkillRoots()
	wantSkills := []string{filepath.Join(root, grantedID, "skills")}
	if !slices.Equal(skills, wantSkills) {
		t.Fatalf("SkillRoots = %v, want %v", skills, wantSkills)
	}
	hooks := host.HookFiles()
	wantHooks := []string{filepath.Join(root, grantedID, "hooks", "on_start.json")}
	if !slices.Equal(hooks, wantHooks) {
		t.Fatalf("HookFiles = %v, want %v", hooks, wantHooks)
	}

	// Node contributions follow the same gate: the ungranted plugin's
	// declarations are visible to no one.
	entries, err := host.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Entries = %d, want both plugins", len(entries))
	}
	for _, entry := range entries {
		want := entry.ID == grantedID
		if got := entry.Manifest.HasPermission("nodes:provide"); got != want {
			t.Fatalf("plugin %s nodes:provide = %v, want %v", entry.ID, got, want)
		}
	}
}

// TestDisabledPluginContributesNothing covers the other fail-closed
// half: disabling a plugin withdraws its skills and hooks even though
// its permissions still authorize them.
func TestDisabledPluginContributesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const id = "hello"
	writePlugin(t, root, id, contributionManifest(
		id, "skills:provide", "hooks:provide"))
	writeContributionTree(t, root, id)
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := NewHost(HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.SetEnabled(context.Background(), id, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if got := host.SkillRoots(); len(got) != 0 {
		t.Fatalf("SkillRoots of a disabled plugin = %v, want none", got)
	}
	if got := host.HookFiles(); len(got) != 0 {
		t.Fatalf("HookFiles of a disabled plugin = %v, want none", got)
	}
}
