package route

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
)

type GenerateSelector interface {
	SelectGenerate(context.Context, inference.GenerateRequest) (Decision, error)
}

// GenerateFallbackPolicy chooses another exact target after a structured,
// transport-safe failed attempt. Both the request and attempt are snapshots;
// returning ok=false stops fallback.
type GenerateFallbackPolicy interface {
	NextGenerate(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error)
}

func (r *Router) Generate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, Trace, error) {
	ctx, span := startRouteSpan(ctx, inference.OperationGenerate)
	response, trace, err := executeWithFallback(r,
		ctx,
		inference.OperationGenerate,
		request.Clone(),
		inference.GenerateRequest.Clone,
		inference.GenerateRequest.Validate,
		r.selectors.Generate,
		func(ctx context.Context, snapshot inference.GenerateRequest) (Decision, error) {
			return r.selectors.Generate.SelectGenerate(ctx, snapshot)
		},
		generateFallbackNext(r.selectors.GenerateFallback),
		func(ctx context.Context, target inference.ModelRef, snapshot inference.GenerateRequest) error {
			_, err := r.target.ExplainGenerate(ctx, target, snapshot)
			return err
		},
		func(ctx context.Context, target inference.ModelRef, snapshot inference.GenerateRequest) (inference.GenerateResponse, inference.Metadata, error) {
			response, err := r.target.Generate(ctx, target, snapshot)
			return response, response.Metadata, err
		},
	)
	recordRoute(ctx, span, inference.OperationGenerate, trace, response.Metadata, err)
	return response, trace, err
}

func (r *Router) GenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (stream inference.GenerateStream, routeTrace Trace, err error) {
	ctx, span := startRouteSpan(ctx, inference.OperationGenerate)
	snapshot := request.Clone()
	stream, routeTrace, err = openSessionWithFallback(r,
		ctx,
		inference.OperationGenerate,
		snapshot,
		inference.GenerateRequest.Clone,
		inference.GenerateRequest.Validate,
		r.selectors.Generate,
		func(ctx context.Context, snapshot inference.GenerateRequest) (Decision, error) {
			return r.selectors.Generate.SelectGenerate(ctx, snapshot)
		},
		generateFallbackNext(r.selectors.GenerateFallback),
		func(ctx context.Context, target inference.ModelRef, snapshot inference.GenerateRequest) error {
			_, err := r.target.ExplainGenerateStream(ctx, target, snapshot)
			return err
		},
		func(ctx context.Context, target inference.ModelRef, snapshot inference.GenerateRequest) (inference.GenerateStream, error) {
			return r.target.GenerateStream(ctx, target, snapshot)
		},
	)
	if err != nil {
		recordRoute(
			ctx, span, inference.OperationGenerate, routeTrace,
			inference.Metadata{}, err,
		)
		return stream, routeTrace, err
	}
	stream = wrapRouteStream(
		ctx, span, inference.OperationGenerate, routeTrace, stream)
	return stream, routeTrace, err
}

// ExplainGenerate explains the selected target's unary Generate compilation.
func (r *Router) ExplainGenerate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := r.selectGenerate(ctx, snapshot)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.target.ExplainGenerate(ctx, decision.Selected, snapshot)
	return explanation, decision, err
}

// ExplainGenerateStream explains the selected target's Generate stream
// compilation without provider I/O.
func (r *Router) ExplainGenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Explanation, Decision, error) {
	snapshot := request.Clone()
	decision, err := r.selectGenerate(ctx, snapshot)
	if err != nil {
		return inference.Explanation{}, Decision{}, err
	}
	explanation, err := r.target.ExplainGenerateStream(
		ctx,
		decision.Selected,
		snapshot,
	)
	return explanation, decision, err
}

func (r *Router) selectGenerate(
	ctx context.Context,
	snapshot inference.GenerateRequest,
) (Decision, error) {
	return selectTarget(
		ctx,
		r.target,
		inference.OperationGenerate,
		snapshot,
		inference.GenerateRequest.Clone,
		inference.GenerateRequest.Validate,
		r.selectors.Generate,
		func(ctx context.Context, snapshot inference.GenerateRequest) (Decision, error) {
			return r.selectors.Generate.SelectGenerate(ctx, snapshot)
		},
	)
}

// generateFallbackNext adapts the configured policy to the generic fallback
// engine; nil disables fallback for the operation.
func generateFallbackNext(
	policy GenerateFallbackPolicy,
) func(context.Context, inference.GenerateRequest, Attempt) (inference.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextGenerate
}
