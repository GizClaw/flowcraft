package route

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
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
	) (model.ModelRef, bool, error)
}

func (r *Router) Generate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, Trace, error) {
	ctx, span := startRouteSpan(ctx, model.OperationGenerate)
	response, trace, err := runAttempts(r, ctx,
		attemptPlan[inference.GenerateRequest, inference.GenerateResponse]{
			operation: model.OperationGenerate,
			snapshot:  request.Clone(),
			clone:     inference.GenerateRequest.Clone,
			validate:  inference.GenerateRequest.Validate,
			selector:  r.selectors.Generate,
			selectRequest: func(
				ctx context.Context, snapshot inference.GenerateRequest,
			) (Decision, error) {
				return r.selectors.Generate.SelectGenerate(ctx, snapshot)
			},
			fallbackNext: generateFallbackNext(r.selectors.GenerateFallback),
			// The preflight binds the target and compiles the request; the
			// work then executes that very compilation, so a routed attempt
			// opens and compiles once instead of twice.
			prepare: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.GenerateRequest,
			) (*inference.Prepared[inference.GenerateResponse], error) {
				return r.target.PrepareGenerate(ctx, target, snapshot)
			},
			work: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.GenerateRequest,
				prepared *inference.Prepared[inference.GenerateResponse],
			) (inference.GenerateResponse, inference.Metadata, error) {
				response, err := prepared.Execute(ctx)
				if err != nil {
					return inference.GenerateResponse{}, inference.Metadata{}, err
				}
				return response, response.Metadata, nil
			},
			phase:     AttemptPhaseExecute,
			outcome:   AttemptOutcomeSucceeded,
			completed: true,
		})
	recordRoute(ctx, span, model.OperationGenerate, trace, response.Metadata, err)
	return response, trace, err
}

func (r *Router) GenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (stream inference.GenerateStream, routeTrace Trace, err error) {
	ctx, span := startRouteSpan(ctx, model.OperationGenerate)
	snapshot := request.Clone()
	stream, routeTrace, err = runAttempts(r, ctx,
		attemptPlan[inference.GenerateRequest, inference.GenerateStream]{
			operation: model.OperationGenerate,
			snapshot:  snapshot,
			clone:     inference.GenerateRequest.Clone,
			validate:  inference.GenerateRequest.Validate,
			selector:  r.selectors.Generate,
			selectRequest: func(
				ctx context.Context, snapshot inference.GenerateRequest,
			) (Decision, error) {
				return r.selectors.Generate.SelectGenerate(ctx, snapshot)
			},
			fallbackNext: generateFallbackNext(r.selectors.GenerateFallback),
			prepare: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.GenerateRequest,
			) (*inference.Prepared[inference.GenerateStream], error) {
				return r.target.PrepareGenerateStream(ctx, target, snapshot)
			},
			work: func(
				ctx context.Context,
				target model.ModelRef,
				snapshot inference.GenerateRequest,
				prepared *inference.Prepared[inference.GenerateStream],
			) (inference.GenerateStream, inference.Metadata, error) {
				stream, err := prepared.Execute(ctx)
				return stream, inference.Metadata{}, err
			},
			phase:   AttemptPhaseOpen,
			outcome: AttemptOutcomeOpened,
		})
	if err != nil {
		recordRoute(
			ctx, span, model.OperationGenerate, routeTrace,
			inference.Metadata{}, err,
		)
		return stream, routeTrace, err
	}
	stream = wrapRouteStream(
		ctx, span, model.OperationGenerate, routeTrace, stream)
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
		model.OperationGenerate,
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
) func(context.Context, inference.GenerateRequest, Attempt) (model.ModelRef, bool, error) {
	if isNilInterface(policy) {
		return nil
	}
	return policy.NextGenerate
}
