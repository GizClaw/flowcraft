package port

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// Materializer turns fused candidates back into grounded
// ContextItems by looking up the canonical fact in the temporal
// store and attaching embedded evidence. Materialization is also
// the read-path's stale-fact filter.
type Materializer interface {
	Materialize(ctx context.Context, candidates []domain.Candidate) ([]domain.ContextItem, []diagnostic.CandidateDrop, error)
}
