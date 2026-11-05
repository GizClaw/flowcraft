package protocol

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/inference"
)

// BindProvider assembles the host-facing contract from the bound halves. Either
// half may be nil: a provider that cannot stream reports streaming as an
// unsupported operation instead of forcing every driver to carry a stub.
func BindProvider(unary GenerateDriver, stream GenerateStreamDriver) inference.Provider {
	return provider{unary: unary, stream: stream}
}

// BindProviderWithCatalog is [BindProvider] plus the model declarations a driver
// publishes. The returned value still satisfies [inference.Provider]; a host
// that wants the declarations asserts [inference.Catalog].
func BindProviderWithCatalog(
	unary GenerateDriver,
	stream GenerateStreamDriver,
	catalog inference.Catalog,
) inference.Provider {
	return catalogProvider{
		Provider: BindProvider(unary, stream),
		Catalog:  catalog,
	}
}

type provider struct {
	unary  GenerateDriver
	stream GenerateStreamDriver
}

func (p provider) Generate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	if p.unary == nil {
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnsupportedOperation,
			inference.OperationGenerate,
			"",
			errors.New("provider does not serve unary generation"),
		)
	}
	return p.unary.Execute(ctx, request)
}

func (p provider) GenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Stream, error) {
	if p.stream == nil {
		return nil, inference.NewError(
			inference.UnsupportedOperation,
			inference.OperationGenerate,
			inference.FieldStream,
			errors.New("provider does not serve streaming generation"),
		)
	}
	return p.stream.Open(ctx, request)
}

type catalogProvider struct {
	inference.Provider
	inference.Catalog
}
