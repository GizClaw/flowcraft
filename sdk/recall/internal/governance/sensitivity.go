package governance

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// SensitivityPolicy redacts or rejects sensitive facts before they
// reach the canonical store. Phase 8 ships a no-op.
type SensitivityPolicy interface {
	Apply(ctx context.Context, scope model.Scope, f model.TemporalFact) (model.TemporalFact, bool)
}

// NopSensitivity allows every fact through unchanged.
type NopSensitivity struct{}

func (NopSensitivity) Apply(_ context.Context, _ model.Scope, f model.TemporalFact) (model.TemporalFact, bool) {
	return f, true
}
