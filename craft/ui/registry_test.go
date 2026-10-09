package ui

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// busEmitter is what craft.Craft is to a registry: a publisher of
// craft-plane events that owns the bus.
type busEmitter struct{ bus event.Bus }

func (e busEmitter) Emit(
	ctx context.Context,
	subject event.Subject,
	payload any,
) error {
	envelope, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return err
	}
	return e.bus.Publish(ctx, envelope)
}

type fixture struct {
	root     string
	store    *plugin.Store
	host     *plugin.Host
	registry *Registry
	events   <-chan event.Envelope
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
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
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	subscription, err := bus.Subscribe(
		context.Background(), event.Pattern(SubjectChanged),
		event.WithBufferSize(32))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	registry, err := NewRegistry(host, busEmitter{bus: bus})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return &fixture{
		root:     root,
		store:    store,
		host:     host,
		registry: registry,
		events:   subscription.C(),
	}
}

// nextChange returns the next craft.ui.changed event.
func (f *fixture) nextChange(t *testing.T) ChangedEvent {
	t.Helper()
	select {
	case envelope := <-f.events:
		if envelope.Subject != SubjectChanged {
			t.Fatalf("subject = %q, want %q", envelope.Subject, SubjectChanged)
		}
		var changed ChangedEvent
		if err := envelope.Decode(&changed); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		return changed
	case <-time.After(5 * time.Second):
		t.Fatal("no craft.ui.changed event")
		return ChangedEvent{}
	}
}

func (f *fixture) assertNoChange(t *testing.T) {
	t.Helper()
	select {
	case envelope := <-f.events:
		t.Fatalf("unexpected %s event", envelope.Subject)
	case <-time.After(100 * time.Millisecond):
	}
}

// writeTree writes a plugin directory with a UI bundle.
func writeTree(t *testing.T, dir, id, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o700); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}
	manifest := `{
		"id": "` + id + `", "name": "Hello", "version": "` + version + `",
		"permissions": ["ui:webview"],
		"ui": {"entry": "dist/index.js"}
	}`
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "dist", "index.js"),
		[]byte("export default "+version+";\n"), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
}

func TestRegistryEntriesAndAssets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	writeTree(t, filepath.Join(f.root, "hello"), "hello", "0.1.0")
	if err := os.MkdirAll(filepath.Join(f.root, "headless"), 0o700); err != nil {
		t.Fatalf("mkdir headless: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(f.root, "headless", "plugin.json"),
		[]byte(`{"id": "headless", "version": "0.2.0"}`), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	entries, err := f.registry.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Entries = %+v, want 2 plugins", entries)
	}
	// Sorted by id, with the revision the read started at.
	if entries[0].ID != "headless" || entries[1].ID != "hello" {
		t.Fatalf("Entries order = %s, %s", entries[0].ID, entries[1].ID)
	}
	hello := entries[1]
	if hello.Name != "Hello" || hello.Version != "0.1.0" ||
		hello.Entry != "dist/index.js" || !hello.Enabled ||
		len(hello.Permissions) != 1 || hello.Permissions[0] != "ui:webview" {
		t.Fatalf("hello entry = %+v", hello)
	}
	if hello.Revision != f.store.Revision() ||
		entries[0].Revision != f.store.Revision() {
		t.Fatalf("entry revisions = %d/%d, want %d",
			entries[0].Revision, hello.Revision, f.store.Revision())
	}
	if entries[0].Entry != "" || entries[0].Permissions != nil {
		t.Fatalf("headless entry carries UI fields: %+v", entries[0])
	}

	assets, ok := f.registry.Assets("hello")
	if !ok {
		t.Fatal("Assets(hello) = false, want a bundle")
	}
	index, err := fs.ReadFile(assets, "index.js")
	if err != nil || string(index) != "export default 0.1.0;\n" {
		t.Fatalf("bundle index.js = %q, %v", index, err)
	}
	if _, ok := f.registry.Assets("headless"); ok {
		t.Error("Assets(headless) = true, want false")
	}
	if _, ok := f.registry.Assets("absent"); ok {
		t.Error("Assets(absent) = true, want false")
	}

	// Disabling is what a shell sees next: the entry flips, the bundle
	// stays readable (the shell is the gate), and the revision moves.
	if err := f.host.SetEnabled(context.Background(), "hello", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if changed := f.nextChange(t); changed.Revision != f.store.Revision() {
		t.Fatalf("change revision = %d, want %d", changed.Revision, f.store.Revision())
	}
	entries, err = f.registry.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if entries[1].Enabled {
		t.Fatalf("hello entry = %+v, want disabled", entries[1])
	}
	if _, ok := f.registry.Assets("hello"); !ok {
		t.Error("Assets(hello) = false while disabled, want the bundle")
	}
}

func TestRegistryPublishesEveryPluginChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	source := t.TempDir()
	install := func(version string) {
		t.Helper()
		dir := filepath.Join(source, "hello")
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("reset source: %v", err)
		}
		writeTree(t, dir, "hello", version)
		if _, err := f.store.Install(ctx, dir); err != nil {
			t.Fatalf("install %s: %v", version, err)
		}
	}
	changed := func(step string) {
		t.Helper()
		if event := f.nextChange(t); event.Revision != f.store.Revision() {
			t.Fatalf("%s: revision = %d, want %d",
				step, event.Revision, f.store.Revision())
		}
	}

	install("0.1.0")
	changed("install")
	install("0.2.0")
	changed("update")
	if _, err := f.store.Rollback(ctx, "hello"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	changed("rollback")
	if err := f.host.SetEnabled(ctx, "hello", false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	changed("disable")
	if err := f.host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	changed("enable")

	// Exactly one event per change: a shell that reloads on every event
	// must not reload on anything else.
	f.assertNoChange(t)
	entries, err := f.registry.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 1 || entries[0].Version != "0.1.0" || !entries[0].Enabled {
		t.Fatalf("entries after the round trip = %+v", entries)
	}

	// A closed registry stops publishing; the store keeps working.
	if err := f.registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.registry.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := f.host.SetEnabled(ctx, "hello", false); err != nil {
		t.Fatalf("SetEnabled after Close: %v", err)
	}
	f.assertNoChange(t)
}

func TestRegistryWithoutAssetHost(t *testing.T) {
	t.Parallel()
	host := &listOnlyHost{}
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	subscription, err := bus.Subscribe(context.Background(), event.Pattern(SubjectChanged))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	registry, err := NewRegistry(host, busEmitter{bus: bus})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	entries, err := registry.Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("Entries = %+v, %v", entries, err)
	}
	// A host that cannot serve assets is not an error: only a shell
	// that draws a UI needs the bundle half.
	if _, ok := registry.Assets("hello"); ok {
		t.Error("Assets = true without an asset host")
	}
	// It is a Host all the same: the revision watch and the event work
	// for anything satisfying the interface, not just plugin.Host.
	host.bump()
	select {
	case envelope := <-subscription.C():
		var changed ChangedEvent
		if err := envelope.Decode(&changed); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if changed.Revision != host.Revision() {
			t.Fatalf("revision = %d, want %d", changed.Revision, host.Revision())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no craft.ui.changed event")
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	host.bump()
	select {
	case envelope := <-subscription.C():
		t.Fatalf("event %s after Close", envelope.Subject)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNewRegistryRequiresHostAndEmitter(t *testing.T) {
	t.Parallel()
	if _, err := NewRegistry(nil, busEmitter{}); err == nil {
		t.Error("NewRegistry(nil host) was accepted")
	}
	if _, err := NewRegistry(&listOnlyHost{}, nil); err == nil {
		t.Error("NewRegistry(nil emitter) was accepted")
	}
}

// listOnlyHost is a Host without the asset half.
type listOnlyHost struct {
	mu       sync.Mutex
	revision uint64
	subs     map[uint64]func()
	next     uint64
}

func (h *listOnlyHost) Entries() ([]plugin.Entry, error) {
	return []plugin.Entry{{
		ID:       "hello",
		Manifest: plugin.Manifest{ID: "hello", Version: "0.1.0"},
		Enabled:  true,
	}}, nil
}

func (h *listOnlyHost) Revision() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.revision
}

func (h *listOnlyHost) Subscribe(fn func()) func() {
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[uint64]func(){}
	}
	h.next++
	id := h.next
	h.subs[id] = fn
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
	}
}

// bump moves the revision and notifies, like a store mutation does.
func (h *listOnlyHost) bump() {
	h.mu.Lock()
	h.revision++
	callbacks := make([]func(), 0, len(h.subs))
	for _, fn := range h.subs {
		callbacks = append(callbacks, fn)
	}
	h.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
}
