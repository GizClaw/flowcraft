package lens

import (
	"fmt"

	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Registry holds registered lenses in planner order.
type Registry struct {
	lenses []Lens
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register appends a lens. Order matches planner source priority.
func (r *Registry) Register(l Lens) {
	if l == nil {
		return
	}
	r.lenses = append(r.lenses, l)
}

// Specs returns LensSpec entries in registration order.
func (r *Registry) Specs() []planner.LensSpec {
	if r == nil {
		return nil
	}
	out := make([]planner.LensSpec, 0, len(r.lenses))
	for _, l := range r.lenses {
		out = append(out, l.Spec())
	}
	return out
}

// BuildAll constructs every registered lens.
func (r *Registry) BuildAll(deps Deps) ([]Built, error) {
	if r == nil {
		return nil, nil
	}
	out := make([]Built, 0, len(r.lenses))
	for _, l := range r.lenses {
		b, err := l.Build(deps)
		if err != nil {
			return nil, fmt.Errorf("lens %s: %w", l.Spec().Name, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// Projections collects non-nil projections in registration order.
func (r *Registry) Projections(built []Built) []port.Projection {
	var out []port.Projection
	for _, b := range built {
		if b.Projection != nil {
			out = append(out, b.Projection)
		}
	}
	return out
}

// Sources collects non-nil sources in registration order.
func (r *Registry) Sources(built []Built) []port.Source {
	var out []port.Source
	for _, b := range built {
		if b.Source != nil {
			out = append(out, b.Source)
		}
	}
	return out
}

// EntitySnapshotter returns the first wired entity snapshotter.
func (r *Registry) EntitySnapshotter(built []Built) port.EntitySnapshotter {
	for _, b := range built {
		if b.EntitySnap != nil {
			return b.EntitySnap
		}
	}
	return nil
}
