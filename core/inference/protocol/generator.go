package protocol

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/inference"
)

// BindGenerator assembles the generation seam from the bound halves. Either
// half may be nil: a provider that cannot stream reports streaming as an
// unsupported operation instead of forcing every driver to carry a stub.
func BindGenerator(unary GenerateDriver, stream GenerateStreamDriver) inference.Generator {
	return generator{unary: unary, stream: stream}
}

type generator struct {
	unary  GenerateDriver
	stream GenerateStreamDriver
}

func (p generator) Generate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	if p.unary == nil {
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnsupportedOperation,
			"",
			errors.New("provider does not serve unary generation"),
		)
	}
	return p.unary.Execute(ctx, request)
}

func (p generator) GenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Stream, error) {
	if p.stream == nil {
		return nil, inference.NewError(
			inference.UnsupportedOperation,
			inference.FieldStream,
			errors.New("provider does not serve streaming generation"),
		)
	}
	return p.stream.Open(ctx, request)
}
