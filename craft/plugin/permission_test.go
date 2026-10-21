package plugin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/hooks"
	"github.com/GizClaw/flowcraft/core/resource"
)

// contributionManifest declares every gated contribution; the
// permissions list is the variable under test.
//
// The run section appears only when tools:provide is granted, because
// that is the one permission whose contribution arrives over a wire: a
// plugin that claims it without a process is a manifest error, and the
// tests that only exercise the manifest gate have no process to start.
func contributionManifest(id string, permissions ...string) string {
	granted := ""
	run := ""
	for i, permission := range permissions {
		if i > 0 {
			granted += ", "
		}
		granted += `"` + permission + `"`
		if permission == "tools:provide" {
			run = `, ` + helperRunJSON(nil)
		}
	}
	return `{
		"id": "` + id + `", "version": "0.1.0",
		"permissions": [` + granted + `],
		"skills": ["skills"],
		"hooks": ["hooks/on_start.json"],
		"nodes": [{"type": "echo", "tool": "node_echo"}]` + run + `
	}`
}

// writeContributionTree lays out the skill and hook files a manifest
// declares, plus the command its run section names: a regular file is
// all a manifest check asks for, and a test that means to start a real
// process replaces it with the helper binary.
func writeContributionTree(t *testing.T, root, dir string) {
	t.Helper()
	for _, rel := range []string{
		filepath.Join("skills", "echo", "SKILL.md"),
		filepath.Join("hooks", "on_start.json"),
		"server",
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
		grantedID, "skills:provide", "hooks:provide", "nodes:provide"))
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
	sources := host.HookSources()
	wantHooks := []hooks.ExtraSource{{
		Path: filepath.Join(root, grantedID, "hooks", "on_start.json"),
		Dir:  filepath.Join(root, grantedID),
	}}
	if !slices.Equal(sources, wantHooks) {
		t.Fatalf("HookSources = %v, want %v", sources, wantHooks)
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

// TestContributionListsReportAnUnreadableState pins what the two
// contribution readers do with a state file they cannot read. The list
// they answer with is the fail-closed one — the same view every other
// reader of that file returns — and they say so, because "this host
// contributes no skills and no hooks" is a claim an application acts
// on, and it is not the same claim as "this host could not tell".
func TestContributionListsReportAnUnreadableState(t *testing.T) {
	sink := installLogSink(t)
	root := t.TempDir()
	const id = "hello"
	writePlugin(t, root, id, contributionManifest(
		id, "skills:provide", "hooks:provide"))
	writeContributionTree(t, root, id)
	stateDir := t.TempDir()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    stateDir,
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := NewHost(HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.SetEnabled(context.Background(), id, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if skills := host.SkillRoots(); len(skills) != 1 {
		t.Fatalf("SkillRoots of a healthy store = %v, want one entry", skills)
	}
	// The damage a crash mid-write leaves behind.
	if err := os.WriteFile(filepath.Join(stateDir, "enabled.json"),
		[]byte(`{"hello": `), 0o600); err != nil {
		t.Fatalf("damage state: %v", err)
	}
	if skills := host.SkillRoots(); len(skills) != 0 {
		t.Fatalf("SkillRoots of an unreadable state = %v, want none", skills)
	}
	if sources := host.HookSources(); len(sources) != 0 {
		t.Fatalf("HookSources of an unreadable state = %v, want none", sources)
	}
	for _, body := range []string{
		"cannot list the skills of the enabled plugins",
		"cannot list the hooks of the enabled plugins",
	} {
		if found := sink.find(body); len(found) == 0 {
			t.Fatalf("no line reported %q", body)
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
	if got := host.HookSources(); len(got) != 0 {
		t.Fatalf("HookSources of a disabled plugin = %v, want none", got)
	}
}

// TestPluginHookSourceRunsAndDrops is the drop rule end to end: a real
// hook runner consumes the plugin's hooks.json while the plugin is
// enabled — anchored to the plugin directory, with the content-bearing
// payload fields stripped — and a runner built after disabling the
// plugin no longer sees the source at all.
func TestPluginHookSourceRunsAndDrops(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const id = "hello"
	writePlugin(t, root, id, contributionManifest(id, "hooks:provide"))
	writeContributionTree(t, root, id)
	pluginDir := filepath.Join(root, id)
	const hookFile = "fired.out"
	hookJSON := `{
		"hooks": {
			"PreToolUse": [{"hooks": [{"command": "cat > ` + hookFile + `"}]}]
		}
	}`
	if err := os.WriteFile(
		filepath.Join(pluginDir, "hooks", "on_start.json"),
		[]byte(hookJSON), 0o600,
	); err != nil {
		t.Fatalf("write plugin hooks.json: %v", err)
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
	// The application side of the seam: build a runner over the
	// host's sources, which is what a runtime generation does.
	buildRunner := func() *hooks.Manager {
		t.Helper()
		value, err := hooks.NewFactory(hooks.WithSources(host)).New(
			context.Background(),
			resource.Input{
				Settings: []byte(`{"path": "` +
					filepath.Join(t.TempDir(), "absent.json") + `"}`),
			},
		)
		if err != nil {
			t.Fatalf("runner factory: %v", err)
		}
		manager := value
		return manager
	}

	runner := buildRunner()
	runner.Fire(context.Background(), hooks.EventPreToolUse, map[string]any{
		"tool":       "exec_command",
		"tool_input": map[string]any{"command": "secret"},
	})
	fired := filepath.Join(pluginDir, hookFile)
	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		if data, err = os.ReadFile(fired); err == nil && len(data) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(data) == 0 {
		// The command writes into the plugin directory, so a file
		// anywhere else would mean the source lost its Dir.
		t.Fatalf("plugin hook did not run in its plugin dir: %v", err)
	}
	if strings.Contains(string(data), "tool_input") {
		t.Fatalf("plugin hook payload kept a content field: %s", data)
	}

	if err := host.SetEnabled(context.Background(), id, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := os.Remove(fired); err != nil {
		t.Fatalf("remove hook output: %v", err)
	}
	buildRunner().Fire(context.Background(), hooks.EventPreToolUse, map[string]any{
		"tool": "exec_command",
	})
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(fired); !os.IsNotExist(err) {
		t.Fatal("a disabled plugin's hook source was still consumed")
	}
}
