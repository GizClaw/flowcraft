package governance

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// NopRetention allows every fact.
type NopRetention struct{}

func (NopRetention) Allow(context.Context, domain.Scope, domain.TemporalFact, time.Time) bool {
	return true
}
