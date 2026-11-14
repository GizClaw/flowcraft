package protocol

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/inference"
)

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

// prepare is the shared front half of both pipelines: validate the request,
// compile it, and prove the compilation accounted for every active field.
//
// It is unexported on purpose. A driver hands over three functions and receives
// a working driver; it cannot call prepare, cannot hold a prepared request, and
// therefore cannot execute one request with another request's compilation.
func prepare[Req Request, Wire any](
	ctx context.Context,
	providerID string,
	compile Compiler[Req, Wire],
	request Req,
	shape Shape,
) (Compiled[Wire], error) {
	if err := request.Validate(); err != nil {
		return Compiled[Wire]{}, err
	}
	compiled, err := compile(ctx, request, shape)
	if err != nil {
		return Compiled[Wire]{}, compilerError(err)
	}
	if err := checkContract(request, compiled.Fields, compiled.Warnings); err != nil {
		return Compiled[Wire]{}, err
	}
	return compiled, nil
}

// compilerError keeps a driver's classified rejection and treats anything else
// as a driver bug: an unclassified compiler failure means the pipeline cannot
// tell a rejected request from a broken driver.
func compilerError(err error) error {
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		return inferenceErr
	}
	return contractError("", "compiler failed without classifying the error")
}

// transportError keeps a driver's classified failure, maps cancellation onto
// OperationInterrupted, and treats the rest as a provider failure so retry
// policy sees a transient class rather than an internal one.
func transportError(ctx context.Context, err error) error {
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		return inferenceErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return inference.NewError(
			inference.OperationInterrupted,
			"",
			contextErr,
		)
	}
	return inference.NewError(
		inference.ProviderFailure,
		"",
		err,
	)
}

// decodeError treats an unclassified decode failure as a malformed provider
// answer: the call finished, the payload did not make sense.
func decodeError(err error) error {
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		return inferenceErr
	}
	return inference.NewError(
		inference.InvalidProviderResponse,
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
