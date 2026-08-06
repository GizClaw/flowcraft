package route

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/inference"
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
	response, trace, err := executeWithFallback(
		ctx,
		r.runtime,
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
			_, err := r.runtime.ExplainGenerate(ctx, target, snapshot)
			return err
		},
		func(ctx context.Context, target inference.ModelRef, snapshot inference.GenerateRequest) (inference.GenerateResponse, inference.Metadata, error) {
			response, err := r.runtime.Generate(ctx, target, snapshot)
			return response, response.Metadata, err
		},
	)
	recordRoute(ctx, span, inference.OperationGenerate, trace, err)
	return response, trace, err
}

func (r *Router) GenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (stream inference.GenerateStream, routeTrace Trace, err error) {
	ctx, span := startRouteSpan(ctx, inference.OperationGenerate)
	defer func() { recordRoute(ctx, span, inference.OperationGenerate, routeTrace, err) }()
	snapshot := request.Clone()
	decision, err := r.selectGenerate(ctx, snapshot)
	if err != nil {
		return nil, Trace{}, err
	}
	fallbackNext := generateFallbackNext(r.selectors.GenerateFallback)
	trace := Trace{Decision: decision}
	target := decision.Selected
	trigger := AttemptTriggerSelection
	seen := map[inference.ModelRef]struct{}{target: {}}
	for {
		if _, err := r.runtime.ExplainGenerateStream(ctx, target, snapshot); err != nil {
			attempt := failedAttempt(target, AttemptPhasePreflight, trigger, err)
			trace.Attempts = append(trace.Attempts, attempt)
			next, ok, fallbackErr := nextFallbackTarget(
				ctx, r.runtime, inference.OperationGenerate,
				snapshot, inference.GenerateRequest.Clone,
				attempt, &trace, seen, fallbackNext,
			)
			if fallbackErr != nil {
				return nil, trace, fallbackErr
			}
			if !ok {
				return nil, trace, err
			}
			target, trigger = next, AttemptTriggerFallback
			continue
		}
		trace.Attempts = append(trace.Attempts, Attempt{
			Target: target, Phase: AttemptPhasePreflight, Trigger: trigger,
			Outcome: AttemptOutcomeSucceeded,
		})

		stream, err := r.runtime.GenerateStream(ctx, target, snapshot)
		if err != nil {
			attempt := failedAttempt(target, AttemptPhaseOpen, trigger, err)
			trace.Attempts = append(trace.Attempts, attempt)
			next, ok, fallbackErr := nextFallbackTarget(
				ctx, r.runtime, inference.OperationGenerate,
				snapshot, inference.GenerateRequest.Clone,
				attempt, &trace, seen, fallbackNext,
			)
			if fallbackErr != nil {
				return nil, trace, fallbackErr
			}
			if !ok {
				return nil, trace, err
			}
			target, trigger = next, AttemptTriggerFallback
			continue
		}
		trace.Attempts = append(trace.Attempts, Attempt{
			Target: target, Phase: AttemptPhaseOpen, Trigger: trigger,
			Outcome: AttemptOutcomeOpened,
		})
		trace.Executed = target
		return stream, trace, nil
	}
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
	explanation, err := r.runtime.ExplainGenerate(ctx, decision.Selected, snapshot)
	return explanation, decision, err
}

func (r *Router) selectGenerate(
	ctx context.Context,
	snapshot inference.GenerateRequest,
) (Decision, error) {
	return selectTarget(
		ctx,
		r.runtime,
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
