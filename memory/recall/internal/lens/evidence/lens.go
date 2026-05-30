package evidence

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

// Lens is projection-only; the planner never activates a source.
type Lens struct{}

func (Lens) Spec() planner.LensSpec {
	return planner.LensSpec{
		Name:     "evidence",
		Weight:   0,
		Activate: func(_ domain.QueryIntent) bool { return false },
	}
}

func (l Lens) Build(deps lens.Deps) (lens.Built, error) {
	return lens.Built{Projection: New(deps.EvidenceStore)}, nil
}
