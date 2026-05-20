package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Plan runs the planner and seeds the single-scope SubScopeStates
// fast path (Phase D.5 federation_fanout will fan out here).
type Plan struct {
	planner      port.Planner
	graphEnabled bool
}

// NewPlan constructs a Plan stage.
func NewPlan(planner port.Planner, graphEnabled bool) *Plan {
	return &Plan{planner: planner, graphEnabled: graphEnabled}
}

// Name implements pipeline.Stage.
func (Plan) Name() string { return "plan" }

// Run implements pipeline.Stage.
func (s *Plan) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	if state.Intent == nil {
		return diagnostic.PlanDetail{}, nil
	}
	plan, err := s.planner.Plan(ctx, port.PlannerInput{
		Scope:        state.Scope,
		Text:         state.Intent.Text,
		Entities:     state.Intent.Entities,
		Limit:        state.Intent.Limit,
		Subject:      state.Intent.Subject,
		Predicate:    state.Intent.Predicate,
		Object:       state.Intent.Object,
		Kinds:        state.Intent.Kinds,
		TimeRange:    state.Intent.TimeRange,
		GraphEnabled: s.graphEnabled,
		GraphHops:    state.Query.GraphHops,
	})
	if err != nil {
		return diagnostic.PlanDetail{}, err
	}
	state.Plan = &plan
	if state.Trace != nil {
		state.Trace.Plan = plan
	}
	lenses := make([]diagnostic.ActivatedLens, 0, len(plan.SourceOrder))
	for _, name := range plan.SourceOrder {
		lenses = append(lenses, diagnostic.ActivatedLens{
			Lens:        name,
			Weight:      0,
			Budget:      plan.SourceBudgets[name],
			ActivatedBy: "planner",
		})
	}
	return diagnostic.PlanDetail{
		ActivatedLenses: lenses,
		TotalBudget:     plan.TotalCap,
	}, nil
}

var _ pipeline.Stage[*read.ReadState] = (*Plan)(nil)
