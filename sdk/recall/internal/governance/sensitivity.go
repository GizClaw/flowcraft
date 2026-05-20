package governance

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// NopSensitivity allows every fact through unchanged.
type NopSensitivity struct{}

func (NopSensitivity) Apply(_ context.Context, _ domain.Scope, f domain.TemporalFact) (domain.TemporalFact, bool) {
	return f, true
}
