package compiler

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// TimeResolver normalizes ObservedAt / ValidFrom / ValidTo. PR-2 is a
// passthrough that only fills ObservedAt — relative time, ranges and
// uncertainty handling arrive in Phase 4.
type TimeResolver interface {
	Resolve(f model.TemporalFact, now time.Time) model.TemporalFact
}

type passthroughTimeResolver struct{}

func (passthroughTimeResolver) Resolve(f model.TemporalFact, now time.Time) model.TemporalFact {
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	}
	return f
}
