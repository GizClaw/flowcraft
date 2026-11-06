package protocol

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
)

// EmbedderDriver is one bound embedding pipeline.
type EmbedderDriver interface {
	Embed(ctx context.Context, request inference.EmbedRequest) (inference.EmbedResponse, error)
}

// BindEmbedder assembles the embedding pipeline from the same stages
// generation uses: validate, compile, prove the compilation accounted for every
// active field, call, decode, verify the answer.
//
// The stages are shared; only the request, the wire and the response differ.
// That is why this file is short: the accounting that catches dropped options
// lives in prepare and is not reimplemented here.
func BindEmbedder[Wire, Raw any](
	providerID string,
	compile Compiler[inference.EmbedRequest, Wire],
	transport Transport[inference.EmbedRequest, Wire, Raw],
	decode Decoder[inference.EmbedRequest, Raw, inference.EmbedResponse],
) (EmbedderDriver, error) {
	p, err := bind("embed", providerID,
		func(request inference.EmbedRequest) inference.EmbedRequest {
			request.Extensions = request.Extensions.ForProvider(providerID)
			return request
		},
		compile, transport, decode,
		func(request inference.EmbedRequest, response inference.EmbedResponse, warnings []inference.Warning) (inference.EmbedResponse, error) {
			response.Warnings = joinWarnings(warnings, response.Warnings)
			return response, response.ValidateFor(request)
		},
	)
	if err != nil {
		return nil, err
	}
	p.observe = embedObservation()
	return embedDriver[Wire, Raw]{pipeline: p}, nil
}

type embedDriver[Wire, Raw any] struct {
	pipeline[inference.EmbedRequest, Wire, Raw, inference.EmbedResponse]
}

// Embed delegates to the shared unary pipeline.
func (d embedDriver[Wire, Raw]) Embed(ctx context.Context, request inference.EmbedRequest) (inference.EmbedResponse, error) {
	return d.do(ctx, request)
}
