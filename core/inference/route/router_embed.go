package route

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

type EmbedSelector interface {
	SelectEmbed(context.Context, inference.EmbedRequest) (Decision, error)
}

// EmbedFallbackPolicy chooses another exact target after a structured,
// transport-safe failed Embed attempt. Both the request and attempt are
// snapshots; returning ok=false stops fallback.
type EmbedFallbackPolicy interface {
	NextEmbed(
		context.Context,
		inference.EmbedRequest,
		Attempt,
	) (model.ModelRef, bool, error)
}

func (r *Router) Embed(
	ctx context.Context,
	request inference.EmbedRequest,
) (inference.EmbedResponse, Trace, error) {
	ctx, span := startRouteSpan(ctx, model.OperationEmbed)
	response, trace, err := runAttempts(r, ctx,
		attemptPlan[inference.EmbedRequest, inference.EmbedResponse]{
			operation: model.OperationEmbed,
			snapshot:  request.Clone(),
			clone:     inference.EmbedRequest.Clone,
			validate:  inference.EmbedRequest.Validate,
			selector:  r.selectors.Embed,
			selectRequest: func(
				ctx context.Context, snapshot inference.EmbedRequest,
			) (Decision, error) {
				return r.selectors.Embed.SelectEmbed(ctx, snapshot)
			},
			fallbackNext: embedFallbackNext(r.selectors.EmbedFallback),
			work: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.EmbedRequest,
				_ *inference.Prepared[inference.EmbedResponse],
			) (inference.EmbedResponse, inference.Metadata, error) {
				response, err := r.target.Embed(ctx, target, snapshot)
				return response, response.Metadata, err
			},
			phase:     AttemptPhaseExecute,
			outcome:   AttemptOutcomeSucceeded,
			completed: true,
		})
	recordRoute(ctx, span, model.OperationEmbed, trace, response.Metadata, err)
	return response, trace, err
}

func (r *Router) ExplainEmbed(
	ctx context.Context,
	request inference.EmbedRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := selectTarget(
		ctx,
		r.target,
		model.OperationEmbed,
		snapshot,
		inference.EmbedRequest.Clone,
		inference.EmbedRequest.Validate,
		r.selectors.Embed,
		func(ctx context.Context, snapshot inference.EmbedRequest) (Decision, error) {
			return r.selectors.Embed.SelectEmbed(ctx, snapshot)
		},
	)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.target.ExplainEmbed(ctx, decision.Selected, snapshot)
	return explanation, decision, err
}

func embedFallbackNext(
	policy EmbedFallbackPolicy,
) func(context.Context, inference.EmbedRequest, Attempt) (model.ModelRef, bool, error) {
	if ptr.IsNil(policy) {
		return nil
	}
	return policy.NextEmbed
}
