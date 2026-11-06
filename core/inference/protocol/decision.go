package protocol

import (
	"context"
	"errors"

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
	switch {
	case providerID == "":
		return nil, errors.New("bind decider: provider id is required")
	case compile == nil:
		return nil, errors.New("bind decider: compiler is required")
	case transport == nil:
		return nil, errors.New("bind decider: transport is required")
	case decode == nil:
		return nil, errors.New("bind decider: decoder is required")
	}
	return &decideDriver[Wire, Raw]{
		providerID: providerID,
		compile:    compile,
		transport:  transport,
		decode:     decode,
	}, nil
}

type decideDriver[Wire, Raw any] struct {
	providerID string
	compile    Compiler[inference.DecisionRequest, Wire]
	transport  Transport[inference.DecisionRequest, Wire, Raw]
	decode     Decoder[inference.DecisionRequest, Raw, inference.DecisionResponse]
}

// Decide validates, compiles, calls and decodes one request.
func (d *decideDriver[Wire, Raw]) Decide(
	ctx context.Context,
	request inference.DecisionRequest,
) (inference.DecisionResponse, error) {
	request.Extensions = request.Extensions.ForProvider(d.providerID)

	compiled, err := prepare(ctx, d.providerID, d.compile, request, ShapeUnary)
	if err != nil {
		return inference.DecisionResponse{}, err
	}
	raw, err := d.transport(ctx, request, compiled.Wire)
	if err != nil {
		return inference.DecisionResponse{}, transportError(ctx, err)
	}
	response, err := d.decode(ctx, request, raw)
	if err != nil {
		return inference.DecisionResponse{}, decodeError(err)
	}
	response.Warnings = joinWarnings(compiled.Warnings, response.Warnings)
	if err := response.ValidateFor(request); err != nil {
		return inference.DecisionResponse{}, err
	}
	return response, nil
}
