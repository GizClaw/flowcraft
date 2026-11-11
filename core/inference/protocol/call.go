package protocol

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
)

// pipeline is the unary execution every seam runs: normalize the request,
// validate, compile, prove the compilation accounted for every active field,
// call, decode, then hand the seam its one specific step.
//
// It exists because three seams — generation, embedding, decision — were three
// copies of this sequence, differing only in their request and response types.
// The copies would drift: the extension filter, the contract check and the
// error classification are exactly the parts a reimplementation forgets.
type pipeline[Req Request, Wire, Raw, Res any] struct {
	providerID string
	// normalize applies per-seam request preparation: extension filtering,
	// because only the concrete request type exposes its Extensions field.
	normalize func(Req) Req
	compile   Compiler[Req, Wire]
	transport Transport[Req, Wire, Raw]
	decode    Decoder[Req, Raw, Res]
	// finish is the seam-specific tail: merge compiler warnings into the
	// response and apply the response contract. It is a function rather than an
	// interface because warnings live in a field on each response type, and a
	// Warnings()/SetWarnings() pair on three types would be more machinery for
	// the same four lines.
	finish func(request Req, response Res, warnings []inference.Warning) (Res, error)
	// observe is the seam's instrumentation. Set by the Bind* function after
	// bind returns, so the shared constructor stays free of seam-specific
	// accessors.
	observe observation[Req, Res]
}

// bind assembles one unary pipeline. Every stage is required, and so is the
// provider identity: a pipeline missing a stage cannot be repaired at runtime.
func bind[Req Request, Wire, Raw, Res any](
	what, providerID string,
	normalize func(Req) Req,
	compile Compiler[Req, Wire],
	transport Transport[Req, Wire, Raw],
	decode Decoder[Req, Raw, Res],
	finish func(request Req, response Res, warnings []inference.Warning) (Res, error),
) (pipeline[Req, Wire, Raw, Res], error) {
	switch {
	case providerID == "":
		return pipeline[Req, Wire, Raw, Res]{}, fmt.Errorf("bind %s: provider id is required", what)
	case normalize == nil:
		return pipeline[Req, Wire, Raw, Res]{}, fmt.Errorf("bind %s: normalizer is required", what)
	case compile == nil:
		return pipeline[Req, Wire, Raw, Res]{}, fmt.Errorf("bind %s: compiler is required", what)
	case transport == nil:
		return pipeline[Req, Wire, Raw, Res]{}, fmt.Errorf("bind %s: transport is required", what)
	case decode == nil:
		return pipeline[Req, Wire, Raw, Res]{}, fmt.Errorf("bind %s: decoder is required", what)
	case finish == nil:
		return pipeline[Req, Wire, Raw, Res]{}, fmt.Errorf("bind %s: finisher is required", what)
	}
	return pipeline[Req, Wire, Raw, Res]{
		providerID: providerID,
		normalize:  normalize,
		compile:    compile,
		transport:  transport,
		decode:     decode,
		finish:     finish,
	}, nil
}

// do executes one request through the shared stages, instrumented as one
// provider round trip.
//
// The span opens before prepare so a request the compiler rejects still produces
// a span with its error kind: "this call failed in 40µs" and "this call timed out
// after 30s" are different incidents, and only one of them is a provider problem.
func (p pipeline[Req, Wire, Raw, Res]) do(ctx context.Context, request Req) (Res, error) {
	request = p.normalize(request)
	ctx, call := startCall(ctx, p.observe, p.providerID, request, ShapeUnary)
	response, err := p.execute(ctx, request, call)
	call.finish(response, err)
	return response, err
}

// execute runs the pipeline stages.
func (p pipeline[Req, Wire, Raw, Res]) execute(
	ctx context.Context,
	request Req,
	call *call[Res],
) (Res, error) {
	var zero Res
	compiled, err := prepare(ctx, p.providerID, p.compile, request, ShapeUnary)
	if err != nil {
		return zero, err
	}
	if call != nil {
		// A compilation failure never reaches the provider; only a call that
		// crossed the transport boundary belongs in the provider log.
		call.attempted = true
	}
	raw, err := p.transport(ctx, request, compiled.Wire)
	if err != nil {
		return zero, transportError(ctx, err)
	}
	response, err := p.decode(ctx, request, raw)
	if err != nil {
		return zero, decodeError(err)
	}
	return p.finish(request, response, compiled.Warnings)
}
