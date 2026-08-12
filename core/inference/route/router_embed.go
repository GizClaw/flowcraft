package route

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
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
	) (inference.ModelRef, bool, error)
}

func (r *Router) Embed(
	ctx context.Context,
	request inference.EmbedRequest,
) (inference.EmbedResponse, Trace, error) {
	ctx, span := startRouteSpan(ctx, inference.OperationEmbed)
	response, trace, err := executeWithFallback(r,
		ctx,
		inference.OperationEmbed,
		request.Clone(),
		inference.EmbedRequest.Clone,
		inference.EmbedRequest.Validate,
		r.selectors.Embed,
		func(ctx context.Context, snapshot inference.EmbedRequest) (Decision, error) {
			return r.selectors.Embed.SelectEmbed(ctx, snapshot)
		},
		embedFallbackNext(r.selectors.EmbedFallback),
		nil,
		func(ctx context.Context, target inference.ModelRef, snapshot inference.EmbedRequest) (inference.EmbedResponse, inference.Metadata, error) {
			response, err := r.target.Embed(ctx, target, snapshot)
			return response, response.Metadata, err
		},
	)
	recordRoute(ctx, span, inference.OperationEmbed, trace, response.Metadata, err)
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
		inference.OperationEmbed,
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
) func(context.Context, inference.EmbedRequest, Attempt) (inference.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextEmbed
}
