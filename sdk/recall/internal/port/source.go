package port

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
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

// EntitySnapshotter exposes the read-side of an entity projection
// that the write-time ingest pipeline uses to fold freshly-extracted
// mentions into existing canonical forms. Subsystems must NOT use
// reflection / type assertions to detect this capability — implement
// the port and register the implementation explicitly.
type EntitySnapshotter interface {
	Snapshot(scope domain.Scope) []EntitySnapshot
}

// EntitySnapshot is a hint about an entity the canonical projection
// has already seen in this scope. Subsystems consuming the snapshot
// (e.g. the ingest pipeline) match canonical forms case-insensitively
// to fold case / alias drift into the same canonical entity.
type EntitySnapshot struct {
	Canonical string
	Aliases   []string
}
