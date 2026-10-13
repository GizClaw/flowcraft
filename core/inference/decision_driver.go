package inference

import (
	"context"
	"time"
)

// DecisionDriver is the provider SPI for the decision workload: one evidence
// input plus an ordered question list, answered in a single unary call. The
// operation has one execution shape, so unlike generate and transcription
// there is no operations struct to assemble — an opener returns this driver
// directly.
type DecisionDriver interface {
	Explain(context.Context, ModelRef, DecisionRequest) (Explanation, error)
	Prepare(
		context.Context,
		ModelRef,
		DecisionRequest,
	) (*Prepared[DecisionResponse], error)
	Execute(
		context.Context,
		ModelRef,
		DecisionRequest,
	) (DecisionResponse, error)
	inferenceDecisionDriver()
}

// BindDecision binds one decision pipeline. compile is local validation and
// wire construction only; transport owns provider I/O, and decode owns the
// strict reading of the provider's response into the canonical answer set.
func BindDecision[Wire, Raw any](
	compile Compiler[DecisionRequest, Wire],
	transport Transport[Wire, Raw],
	decode Decoder[Raw, DecisionResponse],
) (DecisionDriver, error) {
	bound, err := bindPipeline(
		OperationDecision,
		compile,
		transport,
		decode,
		DecisionRequest.Validate,
		DecisionRequest.ActiveFields,
		func(request DecisionRequest) Extensions { return request.Extensions },
		func(request DecisionRequest, extensions Extensions) DecisionRequest {
			request.Extensions = extensions
			return request
		},
		DecisionRequest.Clone,
		func(request DecisionRequest, response DecisionResponse) error {
			return response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	return &decisionDriver[Wire, Raw]{pipeline: bound}, nil
}

type decisionDriver[Wire, Raw any] struct {
	pipeline *pipeline[DecisionRequest, Wire, Raw, DecisionResponse]
}

func (*decisionDriver[Wire, Raw]) inferenceDecisionDriver() {}

func (d *decisionDriver[Wire, Raw]) Explain(
	ctx context.Context,
	model ModelRef,
	request DecisionRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *decisionDriver[Wire, Raw]) Execute(
	ctx context.Context,
	model ModelRef,
	request DecisionRequest,
) (DecisionResponse, error) {
	prepared, err := d.Prepare(ctx, model, request)
	if err != nil {
		return DecisionResponse{}, err
	}
	return prepared.Execute(ctx)
}

// Prepare compiles one decision request against model and returns a handle
// that executes it without compiling again.
func (d *decisionDriver[Wire, Raw]) Prepare(
	ctx context.Context,
	model ModelRef,
	request DecisionRequest,
) (*Prepared[DecisionResponse], error) {
	compiled, err := d.pipeline.prepare(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return newPrepared(
		model,
		OperationDecision,
		compiled.Report,
		func(runCtx context.Context) (DecisionResponse, error) {
			start := time.Now()
			response, err := d.pipeline.executeCompiled(
				runCtx, model, request, compiled)
			if err != nil {
				return DecisionResponse{}, err
			}
			response.Usage.Model = model
			response.Usage.LatencyMs = time.Since(start).Milliseconds()
			response.Metadata = mergeProviderIDs(
				compiled.Report.Metadata(model),
				response.Metadata,
			)
			return response, nil
		},
	), nil
}
