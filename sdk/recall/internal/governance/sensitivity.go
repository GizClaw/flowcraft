package governance

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// SensitivityPolicy redacts or rejects sensitive facts before they
// reach the canonical store. Phase 8 ships a no-op.
type SensitivityPolicy interface {
	Apply(ctx context.Context, scope domain.Scope, f domain.TemporalFact) (domain.TemporalFact, bool)
}

// NopSensitivity allows every fact through unchanged.
type NopSensitivity struct{}

func (NopSensitivity) Apply(_ context.Context, _ domain.Scope, f domain.TemporalFact) (domain.TemporalFact, bool) {
	return f, true
}
