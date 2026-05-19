package governance

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// RetentionPolicy decides whether a fact is eligible for persistence
// under retention rules. Phase 8 ships a no-op; durable policies
// arrive opt-in without changing the compiler boundary.
type RetentionPolicy interface {
	Allow(ctx context.Context, scope model.Scope, f model.TemporalFact, now time.Time) bool
}

// NopRetention allows every fact.
type NopRetention struct{}

func (NopRetention) Allow(context.Context, model.Scope, model.TemporalFact, time.Time) bool {
	return true
}
