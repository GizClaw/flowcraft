package protocol

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/inference"
)

// GenerateDriver is one bound unary pipeline. It is what [BindGenerate] returns
// and what [BindProvider] wraps into an [inference.Generator].
//
// A driver may implement this and not GenerateStreamDriver: a model that cannot
// stream is a driver detail, and [BindProvider] reports it as a rejected request
// rather than a compile-time guarantee every driver must carry.
type GenerateDriver interface {
	Execute(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error)
}

// BindGenerate assembles one unary pipeline. Every stage is required, and so is
// the provider identity: the pipeline drops extensions addressed to other
// providers before compiling, so a driver never has to filter them itself.
func BindGenerate[Wire, Raw any](
	providerID string,
	compile Compiler[inference.GenerateRequest, Wire],
	transport Transport[inference.GenerateRequest, Wire, Raw],
	decode Decoder[inference.GenerateRequest, Raw, inference.GenerateResponse],
) (GenerateDriver, error) {
	switch {
	case providerID == "":
		return nil, errors.New("bind generate: provider id is required")
	case compile == nil:
		return nil, errors.New("bind generate: compiler is required")
	case transport == nil:
		return nil, errors.New("bind generate: transport is required")
	case decode == nil:
		return nil, errors.New("bind generate: decoder is required")
	}
	p, err := bind("generate", providerID,
		func(request inference.GenerateRequest) inference.GenerateRequest {
			request.Extensions = request.Extensions.ForProvider(providerID)
			return request
		},
		compile, transport, decode,
		func(request inference.GenerateRequest, response inference.GenerateResponse, warnings []inference.Warning) (inference.GenerateResponse, error) {
			response.Warnings = joinWarnings(warnings, response.Warnings)
			return response, response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	return unaryDriver[Wire, Raw]{pipeline: p}, nil
}

type unaryDriver[Wire, Raw any] struct {
	pipeline[inference.GenerateRequest, Wire, Raw, inference.GenerateResponse]
}

// Execute delegates to the shared unary pipeline.
func (d unaryDriver[Wire, Raw]) Execute(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	return d.do(ctx, request)
}
