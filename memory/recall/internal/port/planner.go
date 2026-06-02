package port

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// PlannerInput is the planner contract input.
//
// KnownEntities is the cross-sub-scope merged EntitySnapshot list the read-path
// plan stage assembled from every sub-scope in
// state.Scope.EffectiveFederation(). It is preserved for diagnostics and future
// structured planners; the rule-based planner does not use it for lens boosts.
type PlannerInput struct {
	Scope         domain.Scope
	Text          string
	Entities      []string
	Subject       string
	Predicate     string
	Object        string
	Kinds         []domain.FactKind
	TimeRange     domain.TimeRange
	Features      domain.QueryFeatures
	IntentRoute   domain.IntentRoute
	Limit         int
	GraphEnabled  bool
	GraphHops     int
	KnownEntities []EntitySnapshot
}

// Planner produces a QueryPlan from caller hints.
type Planner interface {
	Plan(ctx context.Context, input PlannerInput) (domain.QueryPlan, error)
}
