package observation

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

type Lens struct{}

func (Lens) Spec() planner.LensSpec {
	return planner.LensSpec{
		Name:     planner.SourceObservation,
		Weight:   planner.WeightObservation,
		Activate: func(domain.QueryIntent) bool { return true },
	}
}

func (Lens) Build(deps lens.Deps) (lens.Built, error) {
	return lens.Built{Source: NewSource(deps.Index)}, nil
}
