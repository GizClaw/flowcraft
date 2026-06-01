package port

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// ObservationProjection is a rebuildable derived view over the Observation
// ledger. It deliberately sits beside Projection because assertion projections
// consume TemporalFact, while observation projections consume raw evidence.
type ObservationProjection interface {
	Name() string
	ProjectObservations(ctx context.Context, observations []domain.Observation) error
	RebuildObservations(ctx context.Context, scope domain.Scope, observations []domain.Observation) error
	ForgetObservations(ctx context.Context, scope domain.Scope, observationIDs []string) error
	ClearObservationScope(ctx context.Context, scope domain.Scope) error
}
