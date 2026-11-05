package protocol

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// Transport is the only stage allowed to perform provider I/O.
//
// It receives the canonical request as well as the compiled wire value because a
// few request-level facts are only expressible on the wire itself: a provider
// escape hatch, a header derived from caller metadata. Passing the request keeps
// those facts out of the Wire type, which stays exactly the provider's own
// request value.
//
// Implementations must be safe for concurrent use.
type Transport[Req Request, Wire, Raw any] func(
	ctx context.Context,
	request Req,
	wire Wire,
) (Raw, error)

// Decoder turns a raw provider answer into a canonical response. It must not
// perform I/O.
//
// It receives the canonical request because some answers do not name themselves:
// an image response carries no model field, so the response identity can only come
// from what was asked for.
//
// Implementations must be safe for concurrent use.
type Decoder[Req Request, Raw, Res any] func(
	ctx context.Context,
	request Req,
	raw Raw,
) (Res, error)

// GenerateDriver is one bound unary pipeline. It is what [BindGenerate] returns
// and what [BindProvider] wraps into an [inference.Provider].
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
	return &unaryDriver[Wire, Raw]{
		providerID: providerID,
		compile:    compile,
		transport:  transport,
		decode:     decode,
	}, nil
}

type unaryDriver[Wire, Raw any] struct {
	providerID string
	compile    Compiler[inference.GenerateRequest, Wire]
	transport  Transport[inference.GenerateRequest, Wire, Raw]
	decode     Decoder[inference.GenerateRequest, Raw, inference.GenerateResponse]
}

// Execute validates, compiles, calls and decodes one request.
func (d *unaryDriver[Wire, Raw]) Execute(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	request.Extensions = request.Extensions.ForProvider(d.providerID)
	compiled, err := prepare(ctx, d.providerID, model.OperationGenerate, d.compile, request, ShapeUnary)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	raw, err := d.transport(ctx, request, compiled.Wire)
	if err != nil {
		return inference.GenerateResponse{}, transportError(ctx, model.OperationGenerate, err)
	}
	response, err := d.decode(ctx, request, raw)
	if err != nil {
		return inference.GenerateResponse{}, decodeError(model.OperationGenerate, err)
	}
	response.Warnings = joinWarnings(compiled.Warnings, response.Warnings)
	if err := response.ValidateFor(request); err != nil {
		return inference.GenerateResponse{}, err
	}
	return response, nil
}

// prepare is the shared front half of both pipelines: validate the request,
// compile it, and prove the compilation accounted for every active field.
//
// It is unexported on purpose. A driver hands over three functions and receives
// a working driver; it cannot call prepare, cannot hold a prepared request, and
// therefore cannot execute one request with another request's compilation.
func prepare[Req Request, Wire any](
	ctx context.Context,
	providerID string,
	operation model.Operation,
	compile Compiler[Req, Wire],
	request Req,
	shape Shape,
) (Compiled[Wire], error) {
	if err := request.Validate(); err != nil {
		return Compiled[Wire]{}, err
	}
	compiled, err := compile(ctx, request, shape)
	if err != nil {
		return Compiled[Wire]{}, compilerError(operation, err)
	}
	if err := checkContract(request, operation, compiled.Fields, compiled.Warnings); err != nil {
		return Compiled[Wire]{}, err
	}
	return compiled, nil
}

// compilerError keeps a driver's classified rejection and treats anything else
// as a driver bug: an unclassified compiler failure means the pipeline cannot
// tell a rejected request from a broken driver.
func compilerError(operation model.Operation, err error) error {
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		return inferenceErr
	}
	return contractError(operation, "", "compiler failed without classifying the error")
}

// transportError keeps a driver's classified failure, maps cancellation onto
// OperationInterrupted, and treats the rest as a provider failure so retry
// policy sees a transient class rather than an internal one.
func transportError(ctx context.Context, operation model.Operation, err error) error {
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		return inferenceErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return inference.NewError(
			inference.OperationInterrupted,
			operation,
			"",
			contextErr,
		)
	}
	return inference.NewError(
		inference.ProviderFailure,
		operation,
		"",
		err,
	)
}

// decodeError treats an unclassified decode failure as a malformed provider
// answer: the call finished, the payload did not make sense.
func decodeError(operation model.Operation, err error) error {
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		return inferenceErr
	}
	return inference.NewError(
		inference.InvalidProviderResponse,
		operation,
		"",
		err,
	)
}

// joinWarnings concatenates compiler warnings with response warnings, keeping
// compiler order first. It never returns a shared backing array.
func joinWarnings(first, second []inference.Warning) []inference.Warning {
	if len(first) == 0 && len(second) == 0 {
		return nil
	}
	joined := make([]inference.Warning, 0, len(first)+len(second))
	joined = append(joined, first...)
	joined = append(joined, second...)
	return joined
}
