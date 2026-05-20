package entity

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/lens"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Lens wires the entity mention projection + source.
type Lens struct{}

// Spec implements lens.Lens.
func (Lens) Spec() planner.LensSpec {
	return planner.LensSpec{
		Name:     planner.SourceEntity,
		Weight:   planner.WeightEntity,
		Activate: func(intent domain.QueryIntent) bool { return len(intent.Entities) > 0 },
	}
}

// Build implements lens.Lens.
func (Lens) Build(_ lens.Deps) (lens.Built, error) {
	p := New()
	return lens.Built{
		Projection: p,
		Source:     NewSource(p),
		EntitySnap: &snapshotter{p},
	}, nil
}

type snapshotter struct{ *Projection }

func (s *snapshotter) Snapshot(scope domain.Scope) []port.EntitySnapshot {
	raw := s.Projection.Snapshot(scope)
	if len(raw) == 0 {
		return nil
	}
	out := make([]port.EntitySnapshot, len(raw))
	for i, r := range raw {
		out[i] = port.EntitySnapshot{Canonical: r.Canonical, Aliases: r.Aliases}
	}
	return out
}

var _ port.EntitySnapshotter = (*snapshotter)(nil)
