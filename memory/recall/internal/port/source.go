package port

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// Source is one read-time channel of candidates feeding fusion.
//
// All six lens-resident sources (retrieval / entity / timeline /
// relation / profile / graph) satisfy this one interface — there
// is intentionally NOT a Source-per-lens declaration. Adding a new
// lens means implementing this contract and registering, not
// declaring a new interface kind.
//
// Implementations are strictly read-only: they MUST NOT mutate
// canonical store or projection state.
type Source interface {
	// Name identifies the source. Must match the constants used by
	// the planner so per-source budgets line up.
	Name() string

	// Query emits candidates for the supplied plan. Implementations
	// should respect plan.SourceBudgets[s.Name()] for fanout cap.
	// A non-nil error on the SourceResult means the source failed
	// but other sources should still be consulted.
	Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult
}
