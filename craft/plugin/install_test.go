package plugin

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
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

// TestUninstallRemovesPluginAndData covers the default policy: the
// plugin directory, its rollback snapshot, its enable state, its KV
// file and its data directory all go, so a reinstall starts clean.
func TestUninstallRemovesPluginAndData(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
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
		t.Fatalf("second install: %v", err)
	}
	kv, err := store.KV("hello")
	if err != nil {
		t.Fatalf("KV: %v", err)
	}
	if err := kv.Set("counter", "7"); err != nil {
		t.Fatalf("KV.Set: %v", err)
	}
	dataDir, err := store.DataDir("hello")
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("data file: %v", err)
	}
	if err := store.SetEnabled("hello", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	revision := store.Revision()

	if err := store.Uninstall(context.Background(), "hello", UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if store.Revision() != revision+1 {
		t.Fatalf("revision = %d, want one bump after %d",
			store.Revision(), revision)
	}
	for _, path := range []string{
		filepath.Join(root, "hello"),
		filepath.Join(root, backupsDir, "hello"),
		filepath.Join(store.Options().StateDir, "kv", "hello.json"),
		filepath.Join(store.Options().DataDirRoot, "hello"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s survived the uninstall: %v", path, err)
		}
	}
	if entries, err := store.Entries(); err != nil || len(entries) != 0 {
		t.Fatalf("Entries after uninstall = %+v, %v", entries, err)
	}

	// Reinstalling the plugin sees none of the previous installation:
	// no KV value, no data file, and the enable state is back to the
	// default (enabled).
	writePlugin(t, source, "hello", manifestFor("hello", "0.1.0"))
	if _, err := store.Install(context.Background(),
		filepath.Join(source, "hello")); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	entry, ok := store.Entry("hello")
	if !ok || !entry.Enabled {
		t.Fatalf("entry after reinstall = %+v, %v; want the default state", entry, ok)
	}
	fresh, err := store.KV("hello")
	if err != nil {
		t.Fatalf("KV after reinstall: %v", err)
	}
	if value, ok := fresh.Get("counter"); ok {
		t.Fatalf("KV after reinstall = %q, %v; want nothing", value, ok)
	}
	dir, err := store.DataDir("hello")
	if err != nil {
		t.Fatalf("DataDir after reinstall: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("data dir after reinstall = %v, %v", entries, err)
	}
}

// TestUninstallKeepsDataOnRequest covers the other half of the policy:
// a caller that keeps plugin state says so, and the values are still
// there after a reinstall.
func TestUninstallKeepsDataOnRequest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	kv, err := store.KV("hello")
	if err != nil {
		t.Fatalf("KV: %v", err)
	}
	if err := kv.Set("counter", "7"); err != nil {
		t.Fatalf("KV.Set: %v", err)
	}
	dataDir, err := store.DataDir("hello")
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("data file: %v", err)
	}

	if err := store.Uninstall(context.Background(), "hello",
		UninstallOptions{KeepKV: true, KeepData: true}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	kept, err := store.KV("hello")
	if err != nil {
		t.Fatalf("KV after uninstall: %v", err)
	}
	if value, ok := kept.Get("counter"); !ok || value != "7" {
		t.Fatalf("KV after uninstall = %q, %v; want the kept value", value, ok)
	}
	if body, err := os.ReadFile(filepath.Join(dataDir, "notes.txt")); err != nil ||
		string(body) != "hi" {
		t.Fatalf("data file after uninstall = %q, %v", body, err)
	}
}

// TestUninstallRefusesBuiltinAndUncoversShadow pins both ends of the
// builtin rule: a builtin plugin cannot be removed (it is not writable),
// and removing the user copy that shadows a builtin uncovers the builtin
// instead of leaving a hole.
func TestUninstallRefusesBuiltinAndUncoversShadow(t *testing.T) {
	t.Parallel()
	builtin := t.TempDir()
	user := t.TempDir()
	writePlugin(t, builtin, "hello", manifestFor("hello", "0.1.0"))
	store := builtinStore(t, builtin, user)

	err := store.Uninstall(context.Background(), "hello", UninstallOptions{})
	if !errdefs.IsForbidden(err) {
		t.Fatalf("Uninstall of a builtin = %v, want Forbidden", err)
	}
	if _, err := os.Stat(filepath.Join(builtin, "hello")); err != nil {
		t.Fatalf("builtin plugin after the refusal: %v", err)
	}

	source := t.TempDir()
	writePlugin(t, source, "hello", manifestFor("hello", "0.2.0"))
	if _, err := store.Install(context.Background(),
		filepath.Join(source, "hello")); err != nil {
		t.Fatalf("shadow install: %v", err)
	}
	if err := store.Uninstall(context.Background(), "hello", UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall of the shadow: %v", err)
	}
	entry, ok := store.Entry("hello")
	if !ok || !entry.Builtin || entry.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after removing the shadow = %+v, %v", entry, ok)
	}
	if entry.ShadowsBuiltin {
		t.Fatalf("builtin still reports a shadow: %+v", entry)
	}
}

// TestUninstallRemovesBrokenPlugin covers the plugin a user most wants
// to remove and the valid-plugin view never shows: a directory whose
// manifest no longer parses. It is addressable by its directory name,
// which is the id the scan reports for it.
func TestUninstallRemovesBrokenPlugin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "broken", "{ not json")
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, ok := store.Entry("broken"); ok {
		t.Fatal("a broken plugin reached the valid-plugin view")
	}
	if err := store.Uninstall(context.Background(), "broken", UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "broken")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("broken plugin survived the uninstall: %v", err)
	}
	if err := store.Uninstall(context.Background(), "missing", UninstallOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("Uninstall of an unknown plugin = %v, want NotFound", err)
	}
	if err := store.Uninstall(context.Background(), "Bad!",
		UninstallOptions{}); !errdefs.IsValidation(err) {
		t.Fatalf("Uninstall with an invalid id = %v, want Validation", err)
	}
}

// TestHostUninstallStopsBeforeRemoving covers the host half: a running
// plugin stops first (its source closes, the stopped callback fires, its
// tools leave the shared set), a plugin that is not running needs no
// stop at all, and either way the store's revision is what the watchers
// see.
func TestHostUninstallStopsBeforeRemoving(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, id := range []string{"hello", "quiet"} {
		writePlugin(t, root, id, contributionManifest(id, "mcp:provide"))
		writeContributionTree(t, root, id)
	}
	store := newStoreOver(t, root)
	factory := newSourceFactory()
	host, err := NewHost(HostOptions{Store: store, NewSource: factory.new})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	var stopped []string
	host.SetPluginHooks(nil, func(id string) { stopped = append(stopped, id) })
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	if err := host.SetEnabled(context.Background(), "quiet", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	// Disabling stops the plugin and reports it through the same
	// callback; the uninstall's own stop is what this test is about.
	stopped = nil
	createdQuiet := factory.created["quiet"]
	closedQuiet := factory.closed["quiet"]
	revision := store.Revision()

	if err := host.Uninstall(context.Background(), "hello",
		UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall of a running plugin: %v", err)
	}
	if factory.closed["hello"] != 1 {
		t.Fatalf("source closes = %d, want 1", factory.closed["hello"])
	}
	if len(stopped) != 1 || stopped[0] != "hello" {
		t.Fatalf("stopped callbacks = %v, want [hello]", stopped)
	}
	if tools := host.ToolSet().Tools(); len(tools) != 0 {
		t.Fatalf("tools after the uninstall = %v, want none", tools)
	}

	if err := host.Uninstall(context.Background(), "quiet",
		UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall of a stopped plugin: %v", err)
	}
	if factory.created["quiet"] != createdQuiet || factory.closed["quiet"] != closedQuiet {
		t.Fatalf("removing a disabled plugin touched its process: %+v", factory)
	}
	if store.Revision() != revision+2 {
		t.Fatalf("revision = %d, want two bumps after %d",
			store.Revision(), revision)
	}
	if entries, err := store.Entries(); err != nil || len(entries) != 0 {
		t.Fatalf("Entries after both uninstalls = %+v, %v", entries, err)
	}
	if err := host.Uninstall(context.Background(), "hello",
		UninstallOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("second Uninstall = %v, want NotFound", err)
	}
}
