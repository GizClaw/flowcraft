package views

import (
	"slices"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Registry stores view descriptors by ID. It is a lightweight catalog, not a
// runtime orchestrator, retrieval binding store, or global lineage validator.
// The zero value is ready to use; mutating methods initialize storage lazily.
type Registry struct {
	mu    sync.RWMutex
	views map[ID]Descriptor
}

// NewRegistry creates an empty in-memory registry.
func NewRegistry() *Registry {
	return &Registry{
		views: make(map[ID]Descriptor),
	}
}

// RegisterView registers a view descriptor. Duplicate IDs are rejected.
func (r *Registry) RegisterView(d Descriptor) error {
	if err := d.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.ensureViewsLocked()
	if _, ok := r.views[d.ID]; ok {
		return errdefs.Validationf("memory/views: duplicate view id %q", d.ID)
	}
	r.views[d.ID] = cloneDescriptor(d)
	return nil
}

// View returns a descriptor by ID.
func (r *Registry) View(id ID) (Descriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	d, ok := r.views[id]
	if !ok {
		return Descriptor{}, false
	}
	return cloneDescriptor(d), true
}

// ListViews returns descriptors sorted by ID.
func (r *Registry) ListViews() []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Descriptor, 0, len(r.views))
	for _, d := range r.views {
		out = append(out, cloneDescriptor(d))
	}
	slices.SortFunc(out, func(a, b Descriptor) int {
		return compareString(string(a.ID), string(b.ID))
	})
	return out
}

func cloneDescriptor(in Descriptor) Descriptor {
	return in
}

func (r *Registry) ensureViewsLocked() {
	if r.views == nil {
		r.views = make(map[ID]Descriptor)
	}
}

func compareString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
