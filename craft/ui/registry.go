// Package ui delivers plugin UI bundles to an application shell.
//
// The split is deliberate and narrow: craft delivers assets and
// lifecycle, the shell decides how a bundle runs. There is no component
// model here — a plugin UI is a file the shell loads into its own JS
// host, registering whatever its host offers — so this package is a
// revision-watching view over the plugin store plus a confined
// read-only view of each bundle, and nothing else.
package ui

import (
	"context"
	"io/fs"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// SubjectChanged is published after the plugin registry moves:
// enable, disable, install, update and rollback all end there. The
// shell reloads every bundle when it sees one.
const SubjectChanged event.Subject = "craft.ui.changed"

// ChangedEvent is the payload of SubjectChanged. The revision is the
// plugin store's own counter, so a shell can tell an event it has
// already handled from a new one, and read the enumeration view for the
// entries the revision describes.
type ChangedEvent struct {
	Revision uint64 `json:"revision"`
}

// Host is the plugin-host surface a registry reads. It is narrower than
// craft.PluginHost on purpose: the registry needs the enumeration view,
// the revision and the change notifications, and nothing about tools or
// plugin processes.
type Host interface {
	Entries() ([]plugin.Entry, error)
	Revision() uint64
	Subscribe(fn func()) func()
}

// AssetHost is the optional half of a host: serving bundle files.
// *plugin.Host implements it. A host without it is not an error —
// Assets reports false for every plugin — because listing plugins does
// not require reading them; only a shell that draws a UI does.
type AssetHost interface {
	AssetFS(id string) (fs.FS, bool)
}

// Emitter publishes one craft-plane event; craft.Craft implements it.
type Emitter interface {
	Emit(ctx context.Context, subject event.Subject, payload any) error
}

// Entry is one plugin as the shell sees it before loading a bundle:
// whether to load it (enabled, permissions) and where to find it. The
// revision is the one the read started at, shared by every entry of
// that read.
type Entry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Version     string   `json:"version,omitempty"`
	Entry       string   `json:"entry,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	Enabled     bool     `json:"enabled"`
	Revision    uint64   `json:"revision"`
}

// Registry is the UI delivery surface of one Craft: the entries a shell
// draws, the bundles it loads, and the events that tell it to do both
// again.
type Registry struct {
	host   Host
	assets AssetHost
	emit   Emitter

	mu     sync.Mutex
	cancel func()
	closed bool
}

// NewRegistry opens a registry over a plugin host and starts watching
// its revision. The host must also be the emitter's subject: craft
// passes itself, so the events land on the craft bus.
func NewRegistry(host Host, emit Emitter) (*Registry, error) {
	if host == nil {
		return nil, errdefs.Validationf("ui registry: host is required")
	}
	if emit == nil {
		return nil, errdefs.Validationf("ui registry: emitter is required")
	}
	registry := &Registry{host: host, emit: emit}
	if assets, ok := host.(AssetHost); ok {
		registry.assets = assets
	}
	registry.cancel = host.Subscribe(registry.notify)
	return registry, nil
}

// Entries returns the enumeration view of every valid plugin, enabled
// or not, with the revision the read started at.
//
// The revision is read first, so a change landing while the entries are
// read is not swallowed: its own SubjectChanged arrives behind this
// read. A shell that subscribes before its first read therefore never
// misses an update, which is the contract the change event exists for.
func (r *Registry) Entries() ([]Entry, error) {
	revision := r.host.Revision()
	entries, err := r.host.Entries()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, Entry{
			ID:          entry.ID,
			Name:        entry.Manifest.Name,
			Version:     entry.Manifest.Version,
			Entry:       entry.Manifest.Entry(),
			Permissions: append([]string(nil), entry.Manifest.Permissions...),
			Enabled:     entry.Enabled,
			Revision:    revision,
		})
	}
	return out, nil
}

// Assets returns the read-only bundle view of one plugin: the directory
// holding its ui.entry, addressed by bundle-relative paths. The second
// result is false for a plugin without a UI, for an unknown id, and for
// a host that cannot serve assets.
//
// The bundle is served whether or not the plugin is enabled and whether
// or not it holds ui:webview — the entries say both, and the shell
// decides. Reading is confined to the bundle directory (see
// plugin.Host.AssetFS).
func (r *Registry) Assets(id string) (fs.FS, bool) {
	if r.assets == nil {
		return nil, false
	}
	return r.assets.AssetFS(id)
}

// Close stops the revision watch. It is idempotent, and the registry
// keeps answering reads afterwards: the store it reads outlives it.
func (r *Registry) Close() error {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.closed = true
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// notify publishes one change event. It runs inside the store's
// mutation path — the caller that enabled or installed a plugin — so it
// stays small: read the revision, hand it to the bus, drop the error. A
// bus failure must not fail the plugin change that caused it.
func (r *Registry) notify() {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	_ = r.emit.Emit(
		context.Background(),
		SubjectChanged,
		ChangedEvent{Revision: r.host.Revision()},
	)
}
