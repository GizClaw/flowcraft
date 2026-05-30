package profile

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

// Lens wires the profile projection + source.
type Lens struct{}

func (Lens) Spec() planner.LensSpec {
	return planner.LensSpec{
		Name:     planner.SourceProfile,
		Weight:   planner.WeightProfile,
		Activate: planner.ActivatesProfile,
	}
}

func (Lens) Build(_ lens.Deps) (lens.Built, error) {
	p := New()
	return lens.Built{Projection: p, Source: NewSource(p)}, nil
}
