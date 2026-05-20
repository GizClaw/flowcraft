package governance

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// RetentionPolicy decides whether a fact is eligible for persistence
// under retention rules. Phase 8 ships a no-op; durable policies
// arrive opt-in without changing the compiler boundary.
type RetentionPolicy interface {
	Allow(ctx context.Context, scope domain.Scope, f domain.TemporalFact, now time.Time) bool
}

// NopRetention allows every fact.
type NopRetention struct{}

func (NopRetention) Allow(context.Context, domain.Scope, domain.TemporalFact, time.Time) bool {
	return true
}
