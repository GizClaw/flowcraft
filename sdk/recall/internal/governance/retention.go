package governance

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// NopRetention allows every fact.
type NopRetention struct{}

func (NopRetention) Allow(context.Context, domain.Scope, domain.TemporalFact, time.Time) bool {
	return true
}
