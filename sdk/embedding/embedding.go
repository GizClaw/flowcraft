package embedding

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrMissingCredentials is returned when a provider cannot be created
// because required credentials (api_key, endpoint, etc.) are missing.
var ErrMissingCredentials = errdefs.Unauthorized(errors.New("embedding: missing required credentials"))

// Embedder generates vector embeddings from text.
// Implementations must be safe for concurrent use.
//
// Embed returns a single embedding for the given text.
// EmbedBatch returns embeddings for multiple texts (order-preserving).
// Implementations that do not support batching may fall back to
// sequential single calls.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// DimensionAware is an optional interface that Embedder implementations
// may implement to report their output vector dimensions.
type DimensionAware interface {
	Dimensions() int
}

// EmbedText calls Embed when emb is non-nil; otherwise returns nil, nil.
func EmbedText(ctx context.Context, emb Embedder, text string) ([]float32, error) {
	if emb == nil {
		return nil, nil
	}
	return emb.Embed(ctx, text)
}
