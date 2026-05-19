// Package source defines the CandidateSource contract that the read
// path uses to gather candidates from projections / retrieval
// backends. Sources are strictly read-only: they MUST NOT mutate
// canonical store or projection state (docs §16 invariant).
package source

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// CandidateSource is one channel of candidates feeding into fusion.
// Implementations are short-lived value types; configuration lives
// on the source struct.
type CandidateSource interface {
	// Name identifies the source. Must match the constants used by
	// the planner so per-source budgets line up.
	Name() string

	// Query emits candidates for the supplied plan. Implementations
	// should respect plan.SourceBudgets[s.Name()] for fanout cap.
	// A non-nil error in the SourceResult means the source failed
	// but other sources should still be consulted.
	Query(ctx context.Context, plan model.QueryPlan) model.SourceResult
}
