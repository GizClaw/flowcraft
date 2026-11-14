package inference

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// Embedder is the embedding seam.
//
// It is an optional interface, like [Catalog]: a provider that cannot embed
// should not carry a stub, and a caller that needs embeddings discovers the
// capability instead of discovering an error. The response shape is why this is
// not [Provider].Generate — vectors are not a message, have no role, no finish
// reason and no stream, and a batch of documents is not a conversation.
type Embedder interface {
	// Embed turns documents into vectors, in input order.
	Embed(ctx context.Context, request EmbedRequest) (EmbedResponse, error)
}

// EmbedRequest is one embedding call.
type EmbedRequest struct {
	// Model names the provider-qualified model that should embed.
	Model model.ModelRef `json:"model"`
	// Input holds one element per document. A document is content, not an
	// utterance: there is no role, and a multimodal model may accept a
	// document made of several parts.
	Input []message.Content `json:"input"`
	// Dimensions asks for a specific vector width (matryoshka truncation on
	// models that support it). Zero means the model's declared default.
	Dimensions int `json:"dimensions,omitempty"`
	// Extensions carries provider-specific options.
	Extensions Extensions `json:"-"`
}

// Validate rejects a request before any provider I/O.
func (r EmbedRequest) Validate() error {
	if err := r.Model.Validate(); err != nil {
		return inferenceErr(InvalidRequest, FieldModel, err)
	}
	if len(r.Input) == 0 {
		return inferenceErr(InvalidRequest, FieldInput, fmt.Errorf("at least one document is required"))
	}
	for i, document := range r.Input {
		if err := document.Validate(); err != nil {
			return inferenceErr(InvalidRequest, FieldInput, fmt.Errorf("document %d: %w", i, err))
		}
	}
	if r.Dimensions < 0 {
		return inferenceErr(InvalidRequest, FieldDimensions, fmt.Errorf("dimensions must not be negative"))
	}
	if err := r.Extensions.Validate(); err != nil {
		return inferenceErr(InvalidExtension, FieldExtensions, err)
	}
	return nil
}

// ActiveFields names the knobs this request uses.
func (r EmbedRequest) ActiveFields() []Field {
	fields := []Field{FieldModel, FieldInput}
	if r.Dimensions > 0 {
		fields = append(fields, FieldDimensions)
	}
	fields = append(fields, r.Extensions.ActiveFields()...)
	return fields
}

// EmbedResponse is one batch of vectors.
type EmbedResponse struct {
	// Vectors holds one vector per input document, in input order.
	Vectors [][]float32 `json:"vectors"`
	// Usage reports what the call consumed.
	Usage usage.Usage `json:"usage,omitzero"`
	// Metadata identifies the call that produced this answer.
	Metadata Metadata `json:"metadata,omitzero"`
	// Warnings reports requested options honoured only in part.
	Warnings []Warning `json:"warnings,omitempty"`
}

// ValidateFor reports whether the answer can be the answer to request: one
// vector per document, uniform width, and the requested width when one was
// asked for.
//
// The width check is the one that matters in practice: a provider that ignores
// Dimensions returns a well-formed answer of the wrong shape, and a caller
// storing those vectors would only discover it at query time.
func (r EmbedResponse) ValidateFor(request EmbedRequest) error {
	if len(r.Vectors) != len(request.Input) {
		return inferenceErr(InvalidProviderResponse, FieldVectors,
			fmt.Errorf("got %d vectors for %d documents", len(r.Vectors), len(request.Input)))
	}
	width := 0
	for i, vector := range r.Vectors {
		if len(vector) == 0 {
			return inferenceErr(InvalidProviderResponse, FieldVectors,
				fmt.Errorf("vector %d is empty", i))
		}
		if width == 0 {
			width = len(vector)
			continue
		}
		if len(vector) != width {
			return inferenceErr(InvalidProviderResponse, FieldVectors,
				fmt.Errorf("vector %d has width %d, want %d", i, len(vector), width))
		}
	}
	if request.Dimensions > 0 && width != request.Dimensions {
		return inferenceErr(InvalidProviderResponse, FieldDimensions,
			fmt.Errorf("got width %d, want the requested %d", width, request.Dimensions))
	}
	for i, warning := range r.Warnings {
		if warning.Message == "" {
			return inferenceErr(InvalidProviderResponse, "", fmt.Errorf("warning %d has no message", i))
		}
	}
	return nil
}
