package relation

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

// Lens wires the relation projection + source.
type Lens struct{}

func (Lens) Spec() planner.LensSpec {
	return planner.LensSpec{
		Name:     planner.SourceRelation,
		Weight:   planner.WeightRelation,
		Activate: planner.ActivatesRelation,
	}
}

func (Lens) Build(_ lens.Deps) (lens.Built, error) {
	p := New()
	return lens.Built{Projection: p, Source: NewSource(p)}, nil
}
