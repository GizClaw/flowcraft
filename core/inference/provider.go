package inference

import (
	"context"
	"io"

	"github.com/GizClaw/flowcraft/core/inference/model"
)

// Provider is the whole seam a driver implements. A host holds one and calls
// it; it never learns which implementation it holds, and an application decides
// that when it constructs the provider.
//
// Implementations must be safe for concurrent use: a host may serve several
// conversations through one provider.
type Provider interface {
	// Generate performs one generation and returns the finished response.
	Generate(ctx context.Context, request GenerateRequest) (GenerateResponse, error)
	// GenerateStream performs one generation and returns its incremental
	// events. The returned stream must be closed by the caller.
	GenerateStream(ctx context.Context, request GenerateRequest) (Stream, error)
}

// Catalog exposes the model facts an application declared for this provider:
// which models exist, what they accept and emit, and their limits. It is
// optional; a driver that ships no model line-up does not implement it.
//
// Catalog is how declared capabilities reach the runtime. Without it the
// application's model declarations would be data with no reader.
type Catalog interface {
	// Models returns every declared model. The slice is owned by the
	// provider and must not be mutated by the caller.
	Models() []model.ModelDescriptor
	// Describe returns the descriptor for one model. found is false when
	// the model is not declared.
	Describe(id model.ModelID) (descriptor model.ModelDescriptor, found bool)
}

// errStreamDone is the sentinel a Stream may return instead of [io.EOF]. It
// exists only to let drivers with a non-io error vocabulary end a stream
// without importing io; either value is accepted by callers.
var errStreamDone = io.EOF
