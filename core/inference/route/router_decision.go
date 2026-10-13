package route

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

type DecisionSelector interface {
	SelectDecision(context.Context, inference.DecisionRequest) (Decision, error)
}

// DecisionFallbackPolicy chooses another exact target after a structured,
// transport-safe failed decision attempt. Both the request and attempt are
// snapshots; returning ok=false stops fallback.
type DecisionFallbackPolicy interface {
	NextDecision(
		context.Context,
		inference.DecisionRequest,
		Attempt,
	) (model.ModelRef, bool, error)
}

func (r *Router) Decision(
	ctx context.Context,
	request inference.DecisionRequest,
) (inference.DecisionResponse, Trace, error) {
	ctx, span := startRouteSpan(ctx, model.OperationDecision)
	response, trace, err := runAttempts(r, ctx,
		attemptPlan[inference.DecisionRequest, inference.DecisionResponse]{
			operation: model.OperationDecision,
			snapshot:  request.Clone(),
			clone:     inference.DecisionRequest.Clone,
			validate:  inference.DecisionRequest.Validate,
			selector:  r.selectors.Decision,
			selectRequest: func(
				ctx context.Context, snapshot inference.DecisionRequest,
			) (Decision, error) {
				return r.selectors.Decision.SelectDecision(ctx, snapshot)
			},
			fallbackNext: decisionFallbackNext(r.selectors.DecisionFallback),
			work: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.DecisionRequest,
				_ *inference.Prepared[inference.DecisionResponse],
			) (inference.DecisionResponse, inference.Metadata, error) {
				response, err := r.target.Decision(ctx, target, snapshot)
				return response, response.Metadata, err
			},
			phase:     AttemptPhaseExecute,
			outcome:   AttemptOutcomeSucceeded,
			completed: true,
		})
	recordRoute(ctx, span, model.OperationDecision, trace, response.Metadata, err)
	return response, trace, err
}

// ExplainDecision explains the selected target's decision compilation without
// provider I/O.
func (r *Router) ExplainDecision(
	ctx context.Context,
	request inference.DecisionRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := selectTarget(
		ctx,
		r.target,
		model.OperationDecision,
		snapshot,
		inference.DecisionRequest.Clone,
		inference.DecisionRequest.Validate,
		r.selectors.Decision,
		func(
			ctx context.Context, snapshot inference.DecisionRequest,
		) (Decision, error) {
			return r.selectors.Decision.SelectDecision(ctx, snapshot)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.target.ExplainDecision(
		ctx,
		decision.Selected,
		snapshot,
	)
	return explanation, decision, err
}

// decisionFallbackNext adapts the configured policy to the generic fallback
// engine; nil disables fallback for the operation.
func decisionFallbackNext(
	policy DecisionFallbackPolicy,
) func(context.Context, inference.DecisionRequest, Attempt) (model.ModelRef, bool, error) {
	if ptr.IsNil(policy) {
		return nil
	}
	return policy.NextDecision
}
