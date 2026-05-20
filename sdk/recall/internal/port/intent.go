package port

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// IntentInput is the read-path query interpretation contract.
// Partitioning (Scope) is applied on the planner / materialize
// path, not here.
type IntentInput struct {
	Text      string
	Entities  []string
	Subject   string
	Predicate string
	Object    string
	Kinds     []domain.FactKind
	TimeRange domain.TimeRange
}

// IntentResult is the structured output fed into Planner.Plan.
// Explicit caller hints win; rule extraction only fills gaps.
type IntentResult struct {
	Text      string
	Entities  []string
	Subject   string
	Predicate string
	Object    string
	Kinds     []domain.FactKind
	TimeRange domain.TimeRange
}

// IntentCompiler enriches a recall.Query before planning. Concrete
// implementations live in internal/intent/.
type IntentCompiler interface {
	Compile(ctx context.Context, input IntentInput) (IntentResult, error)
}
