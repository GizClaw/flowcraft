package port

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// IntentRouterInput is the read-path intent routing contract.
// Partitioning (Scope) is applied on the planner / materialize
// path, not here.
type IntentRouterInput struct {
	Text      string
	Entities  []string
	Subject   string
	Predicate string
	Object    string
	Kinds     []domain.FactKind
	TimeRange domain.TimeRange
}

// IntentRouterResult is the structured output fed into Planner.Plan.
type IntentRouterResult struct {
	Text      string
	Entities  []string
	Subject   string
	Predicate string
	Object    string
	Kinds     []domain.FactKind
	TimeRange domain.TimeRange
	Features  domain.QueryFeatures
	Route     domain.IntentRoute
}

// IntentRouter selects a recall strategy and preserves low-risk literal
// features before planning. Concrete implementations live in internal/intent/.
type IntentRouter interface {
	Route(ctx context.Context, input IntentRouterInput) (IntentRouterResult, error)
}
