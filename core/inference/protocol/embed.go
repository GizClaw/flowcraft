package protocol

import (
	"context"
	"errors"

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
	switch {
	case providerID == "":
		return nil, errors.New("bind embedder: provider id is required")
	case compile == nil:
		return nil, errors.New("bind embedder: compiler is required")
	case transport == nil:
		return nil, errors.New("bind embedder: transport is required")
	case decode == nil:
		return nil, errors.New("bind embedder: decoder is required")
	}
	return &embedDriver[Wire, Raw]{
		providerID: providerID,
		compile:    compile,
		transport:  transport,
		decode:     decode,
	}, nil
}

type embedDriver[Wire, Raw any] struct {
	providerID string
	compile    Compiler[inference.EmbedRequest, Wire]
	transport  Transport[inference.EmbedRequest, Wire, Raw]
	decode     Decoder[inference.EmbedRequest, Raw, inference.EmbedResponse]
}

// Embed validates, compiles, calls and decodes one request.
func (d *embedDriver[Wire, Raw]) Embed(
	ctx context.Context,
	request inference.EmbedRequest,
) (inference.EmbedResponse, error) {
	request.Extensions = request.Extensions.ForProvider(d.providerID)

	compiled, err := prepare(ctx, d.providerID, d.compile, request, ShapeUnary)
	if err != nil {
		return inference.EmbedResponse{}, err
	}
	raw, err := d.transport(ctx, request, compiled.Wire)
	if err != nil {
		return inference.EmbedResponse{}, transportError(ctx, err)
	}
	response, err := d.decode(ctx, request, raw)
	if err != nil {
		return inference.EmbedResponse{}, decodeError(err)
	}
	response.Warnings = joinWarnings(compiled.Warnings, response.Warnings)
	if err := response.ValidateFor(request); err != nil {
		return inference.EmbedResponse{}, err
	}
	return response, nil
}
