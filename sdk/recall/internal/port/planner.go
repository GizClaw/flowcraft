package port

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// PlannerInput is the planner contract input.
type PlannerInput struct {
	Scope        domain.Scope
	Text         string
	Entities     []string
	Subject      string
	Predicate    string
	Object       string
	Kinds        []domain.FactKind
	TimeRange    domain.TimeRange
	Limit        int
	GraphEnabled bool
	GraphHops    int
}

// Planner produces a QueryPlan from caller hints.
type Planner interface {
	Plan(ctx context.Context, input PlannerInput) (domain.QueryPlan, error)
}
