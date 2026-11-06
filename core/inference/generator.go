package inference

import "context"

// Generator is the generation seam. It is what a driver implements and what a
// host calls when it wants a turn: the name says which capability it is, the
// same way Embedder and Decider do.
//
// Implementations must be safe for concurrent use: a host may serve several
// conversations through one generator.
type Generator interface {
	// Generate performs one generation and returns the finished response.
	Generate(ctx context.Context, request GenerateRequest) (GenerateResponse, error)
	// GenerateStream performs one generation and returns its incremental
	// events. The returned stream must be closed by the caller.
	GenerateStream(ctx context.Context, request GenerateRequest) (Stream, error)
}
