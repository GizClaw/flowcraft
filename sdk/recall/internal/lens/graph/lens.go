package graph

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/lens"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
)

// Lens wires the graph projection + source.
type Lens struct{}

func (Lens) Spec() planner.LensSpec {
	return planner.LensSpec{
		Name:     planner.SourceGraph,
		Weight:   planner.WeightGraph,
		Activate: planner.ActivatesGraph,
	}
}

func (Lens) Build(_ lens.Deps) (lens.Built, error) {
	p := New()
	return lens.Built{Projection: p, Source: NewSource(p)}, nil
}
