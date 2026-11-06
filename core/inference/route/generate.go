package route

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Generate routes one unary generation.
func (r *Router) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	return dispatch(ctx, r.targets, r.policy.Generate, request, invokeGenerate, r.policy.Retry)
}

// GenerateStream routes the opening of one stream.
//
// Failover covers only the open: once the stream is returned, an error arrives
// during iteration, after events have already been produced, and replaying it on
// another target would duplicate partial output.
func (r *Router) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	ordered := r.targets
	if r.policy.Generate != nil {
		selected, err := r.policy.Generate.Select(ctx, request, r.targets)
		if err != nil {
			return nil, err
		}
		ordered = selected
	}

	policy := r.policy.Retry.effective(len(ordered))
	trace := TraceFrom(ctx)

	var last error
	tried := 0
	for _, target := range ordered {
		if tried >= policy.MaxTargets {
			break
		}
		tried++
		if target.Provider.Generator == nil {
			continue
		}
		stream, err := target.Provider.GenerateStream(ctx, request)
		if err == nil {
			trace.record(Attempt{Target: target.Name})
			return stream, nil
		}
		last = err
		trace.record(Attempt{Target: target.Name, Kind: kindOf(err), Err: err.Error()})
		if classify(err) == RetryStop {
			return nil, err
		}
	}
	return nil, last
}

// invokeGenerate calls one target's generation seam.
func invokeGenerate(ctx context.Context, target Target, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	if target.Provider.Generator == nil {
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnsupportedOperation, inference.FieldModel,
			fmt.Errorf("route: target %q does not generate", target.Name))
	}
	return target.Provider.Generate(ctx, request)
}

// routedGenerator is the generation seam of a Router.
type routedGenerator struct {
	router *Router
}

func (g routedGenerator) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	return g.router.Generate(ctx, request)
}

func (g routedGenerator) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	return g.router.GenerateStream(ctx, request)
}
