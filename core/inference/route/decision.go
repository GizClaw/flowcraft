package route

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Decide routes one judgment call.
//
// One call is answered by one model: dispatch re-invokes the whole request on
// the next target, never a subset of questions. Mixing answers would combine
// probabilities and confidences from two models, which are not comparable, and a
// refusal from one with an answer from another.
func (r *Router) Decide(ctx context.Context, request inference.DecisionRequest) (inference.DecisionResponse, error) {
	return dispatch(ctx, operationDecide, r.targets, r.policy.Decision, request,
		modelNameOfDecide, withDecideTarget, invokeDecide, r.policy.Retry)
}

// modelNameOfDecide is the model the caller asked to judge.
func modelNameOfDecide(request inference.DecisionRequest) string { return request.Model.ID.Name }

// withDecideTarget stamps the chosen target onto the request.
func withDecideTarget(request inference.DecisionRequest, target Target) inference.DecisionRequest {
	if target.Model.Name != "" {
		request.Model.ID = target.Model
	}
	if request.Model.Profile == "" {
		request.Model.Profile = target.Profile
	}
	return request
}

func invokeDecide(ctx context.Context, target Target, request inference.DecisionRequest) (inference.DecisionResponse, error) {
	if target.Provider.Decider == nil {
		return inference.DecisionResponse{}, inference.NewError(
			inference.UnsupportedOperation, inference.FieldModel,
			fmt.Errorf("route: target %q does not judge", target.Name))
	}
	return target.Provider.Decider.Decide(ctx, request)
}

// routedDecider is the judgment seam of a Router.
type routedDecider struct {
	router *Router
}

func (d routedDecider) Decide(ctx context.Context, request inference.DecisionRequest) (inference.DecisionResponse, error) {
	return d.router.Decide(ctx, request)
}
