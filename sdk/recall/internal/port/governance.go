package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// WritePolicy is the governance hook on the write path. It may
// mutate or reject a fact (privacy / retention / consent).
type WritePolicy interface {
	Apply(f domain.TemporalFact) (domain.TemporalFact, bool)
}

// RetentionPolicy decides whether a fact is eligible for persistence
// under retention rules.
type RetentionPolicy interface {
	Allow(ctx context.Context, scope domain.Scope, f domain.TemporalFact, now time.Time) bool
}

// SensitivityPolicy redacts or rejects sensitive facts before they
// reach the canonical store.
type SensitivityPolicy interface {
	Apply(ctx context.Context, scope domain.Scope, f domain.TemporalFact) (domain.TemporalFact, bool)
}
