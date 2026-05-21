package entity

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// snapshotter adapts the entity Projection to the port.EntitySnapshotter
// contract so the write-time ingest pipeline can fold freshly extracted
// mentions back into existing canonical entities without reaching through
// reflection / type assertion.
type snapshotter struct{ *Projection }

var _ port.EntitySnapshotter = (*snapshotter)(nil)

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
