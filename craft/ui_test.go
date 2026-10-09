package craft

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/craft/plugin"
	"github.com/GizClaw/flowcraft/craft/ui"
)

const uiTestDefinition = `
craft:
  id: test
  version: 0.1.0
deploy:
  version: v1
  resources: {}
`

// TestCraftUIDeliversBundlesAndEvents is the wiring the shell sees: a
// Craft with a plugin host exposes the registry, the registry reads the
// plugin's ui.entry, and a plugin change reaches the shell as a
// craft-plane event — the same bus every other craft event uses, so a
// shell attaches once.
func TestCraftUIDeliversBundlesAndEvents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeUIPlugin(t, root, "hello")
	store, err := plugin.NewStore(plugin.Options{
		Roots:       []plugin.Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := plugin.NewHost(plugin.HostOptions{Store: store})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	def, err := ParseDefinition([]byte(uiTestDefinition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{DataDir: t.TempDir(), Plugins: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx := context.Background()
	// The shell contract: subscribe before the first read, so a change
	// landing between the two cannot be missed.
	changes := make(chan ui.ChangedEvent, 4)
	detach, err := c.Attach(
		ctx,
		event.Pattern(ui.SubjectChanged),
		event.SinkFunc(func(_ context.Context, envelope event.Envelope) error {
			var payload ui.ChangedEvent
			if err := envelope.Decode(&payload); err != nil {
				return err
			}
			changes <- payload
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer detach()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	registry := c.UI()
	if registry == nil {
		t.Fatal("UI() = nil with a plugin host")
	}
	entries, err := registry.Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("Entries = %+v, %v", entries, err)
	}
	if entries[0].ID != "hello" || !entries[0].Enabled ||
		entries[0].Entry != "dist/index.js" {
		t.Fatalf("entry = %+v", entries[0])
	}
	assets, ok := registry.Assets("hello")
	if !ok {
		t.Fatal("Assets(hello) = false, want the bundle")
	}
	bundle, err := fs.ReadFile(assets, "index.js")
	if err != nil || string(bundle) != "export default 1;\n" {
		t.Fatalf("bundle = %q, %v", bundle, err)
	}

	if err := host.SetEnabled(ctx, "hello", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	select {
	case payload := <-changes:
		if payload.Revision != host.Revision() {
			t.Fatalf("change revision = %d, want %d",
				payload.Revision, host.Revision())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no craft.ui.changed event on the craft bus")
	}
	entries, err = registry.Entries()
	if err != nil || len(entries) != 1 || entries[0].Enabled {
		t.Fatalf("entries after disable = %+v, %v", entries, err)
	}

	// Closing stops the watch but does not take the registry away:
	// Plugins() keeps answering too, and what the shell reads out of a
	// closed Craft is the state it ended in.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.UI() == nil {
		t.Fatal("UI() = nil after Close, want the registry it was built with")
	}
	if entries, err = registry.Entries(); err != nil || len(entries) != 1 {
		t.Fatalf("Entries after Close = %+v, %v", entries, err)
	}
	select {
	case payload := <-changes:
		t.Fatalf("event %+v after Close", payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCraftUIWithoutPluginHost(t *testing.T) {
	t.Parallel()
	def, err := ParseDefinition([]byte(uiTestDefinition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if registry := c.UI(); registry != nil {
		t.Fatalf("UI() = %+v without a plugin host, want nil", registry)
	}
}

// TestCraftUIWithAShellLessHost keeps the registry on the narrow
// interface: the plugin host a test double stands in for serves entries
// and events, and simply has no bundles to hand out.
func TestCraftUIWithAShellLessHost(t *testing.T) {
	t.Parallel()
	def, err := ParseDefinition([]byte(uiTestDefinition))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	host := &fakePluginHost{
		set: plugin.NewToolSet(),
		src: &mountSource{},
	}
	c, err := New(def, Options{DataDir: t.TempDir(), Plugins: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	registry := c.UI()
	if registry == nil {
		t.Fatal("UI() = nil with a plugin host")
	}
	entries, err := registry.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Entries = %+v, want none", entries)
	}
	if _, ok := registry.Assets("hello"); ok {
		t.Error("Assets = true for a host that serves no bundles")
	}
}

// writeUIPlugin writes a plugin with a one-file UI bundle.
func writeUIPlugin(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o700); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	manifest := `{
		"id": "` + id + `", "name": "Hello", "version": "0.1.0",
		"permissions": ["ui:webview"],
		"ui": {"entry": "dist/index.js"}
	}`
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "dist", "index.js"),
		[]byte("export default 1;\n"), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
}
