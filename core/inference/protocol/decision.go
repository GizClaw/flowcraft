package protocol

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
)

// DeciderDriver is one bound judgment pipeline.
type DeciderDriver interface {
	Decide(ctx context.Context, request inference.DecisionRequest) (inference.DecisionResponse, error)
}

// BindDecider assembles the judgment pipeline from the same stages generation
// and embedding use: validate, compile, prove the compilation accounted for
// every active field, call, decode, verify the answer.
func BindDecider[Wire, Raw any](
	providerID string,
	compile Compiler[inference.DecisionRequest, Wire],
	transport Transport[inference.DecisionRequest, Wire, Raw],
	decode Decoder[inference.DecisionRequest, Raw, inference.DecisionResponse],
) (DeciderDriver, error) {
	p, err := bind("decide", providerID,
		func(request inference.DecisionRequest) inference.DecisionRequest {
			request.Extensions = request.Extensions.ForProvider(providerID)
			return request
		},
		compile, transport, decode,
		func(request inference.DecisionRequest, response inference.DecisionResponse, warnings []inference.Warning) (inference.DecisionResponse, error) {
			response.Warnings = joinWarnings(warnings, response.Warnings)
			return response, response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	p.observe = decideObservation()
	return decideDriver[Wire, Raw]{pipeline: p}, nil
}

type decideDriver[Wire, Raw any] struct {
	pipeline[inference.DecisionRequest, Wire, Raw, inference.DecisionResponse]
}

// Decide delegates to the shared unary pipeline.
func (d decideDriver[Wire, Raw]) Decide(ctx context.Context, request inference.DecisionRequest) (inference.DecisionResponse, error) {
	return d.do(ctx, request)
}
