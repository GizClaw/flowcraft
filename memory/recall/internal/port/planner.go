package port

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// PlannerInput is the planner contract input.
//
// KnownEntities is the cross-sub-scope merged EntitySnapshot list the
// read-path plan stage assembled from every sub-scope in
// state.Scope.EffectiveFederation() (Cluster G, D2 2026-05-21). The
// planner uses it as a soft "query focus" hint to boost entity-aware
// lenses (entity / relation / graph / profile) when query terms
// intersect the canonical / alias surface. Leaving it empty preserves
// the pre-D2 unweighted plan.
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
	Limit         int
	GraphEnabled  bool
	GraphHops     int
	KnownEntities []EntitySnapshot
}

// Planner produces a QueryPlan from caller hints.
type Planner interface {
	Plan(ctx context.Context, input PlannerInput) (domain.QueryPlan, error)
}
