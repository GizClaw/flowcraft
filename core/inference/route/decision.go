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
	return dispatch(ctx, operationDecide, r.targets, r.policy.Decision, request, invokeDecide, r.policy.Retry)
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
