package openai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// embedCompile maps a request onto embedding parameters.
func (d *driver) embedCompile(
	_ context.Context,
	request inference.EmbedRequest,
	shape protocol.Shape,
) (protocol.Compiled[openai.EmbeddingNewParams], error) {
	if shape != protocol.ShapeUnary {
		return protocol.Compiled[openai.EmbeddingNewParams]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldStream,
			errors.New("openai: embeddings are a unary call"),
		)
	}
	if err := d.checkEmbedDeclarations(request); err != nil {
		return protocol.Compiled[openai.EmbeddingNewParams]{}, err
	}

	documents := make([]string, 0, len(request.Input))
	for i, document := range request.Input {
		var b strings.Builder
		for _, part := range document.Parts {
			text, ok := part.(message.TextPart)
			if !ok {
				return protocol.Compiled[openai.EmbeddingNewParams]{}, inference.NewError(
					inference.UnsupportedFeature,
					inference.FieldInput,
					fmt.Errorf("openai: embeddings take text documents, not %q", part.Kind()),
				)
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(text.Text)
		}
		joined := strings.TrimSpace(b.String())
		if joined == "" {
			return protocol.Compiled[openai.EmbeddingNewParams]{}, inference.NewError(
				inference.InvalidRequest,
				inference.FieldInput,
				fmt.Errorf("openai: document %d is empty", i),
			)
		}
		documents = append(documents, joined)
	}

	params := openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(request.Model.ID.Name),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: documents},
	}
	if request.Dimensions > 0 {
		params.Dimensions = openai.Int(int64(request.Dimensions))
	}

	return protocol.Compiled[openai.EmbeddingNewParams]{
		Wire:   params,
		Fields: embedFields(request),
	}, nil
}

// embedFields names the canonical fields this compilation honoured.
func embedFields(request inference.EmbedRequest) []inference.Field {
	fields := []inference.Field{inference.FieldModel, inference.FieldInput}
	if request.Dimensions > 0 {
		fields = append(fields, inference.FieldDimensions)
	}
	return fields
}

// checkEmbedDeclarations enforces the declared line-up: an undeclared model is
// refused, and a model with no declared embedding width cannot embed.
func (d *driver) checkEmbedDeclarations(request inference.EmbedRequest) error {
	if len(d.settings.Models) == 0 {
		return nil
	}
	descriptor, ok := findModel(d.settings.Models, request.Model.ID)
	if !ok {
		return inference.NewError(
			inference.UnknownModel,
			inference.FieldModel,
			fmt.Errorf("openai: model %q is not declared by this deployment", request.Model.ID.Name),
		)
	}
	if descriptor.Capabilities.EmbeddingDimensions <= 0 {
		return inference.NewError(
			inference.UnsupportedOperation,
			inference.FieldModel,
			fmt.Errorf("openai: model %q declares no embedding width", descriptor.ID.Name),
		)
	}
	return nil
}

// embedCall performs the provider I/O.
func (d *driver) embedCall(
	ctx context.Context,
	request inference.EmbedRequest,
	params openai.EmbeddingNewParams,
) (openai.CreateEmbeddingResponse, error) {
	response, err := d.client.Embeddings.New(ctx, params)
	if err != nil {
		return openai.CreateEmbeddingResponse{}, err
	}
	return *response, nil
}

// embedDecode places each vector at the index the provider reported. The order
// is not assumed: a provider that answers out of order would silently attribute
// one document's vector to another.
func (d *driver) embedDecode(
	_ context.Context,
	request inference.EmbedRequest,
	raw openai.CreateEmbeddingResponse,
) (inference.EmbedResponse, error) {
	vectors := make([][]float32, len(request.Input))
	for _, item := range raw.Data {
		index := int(item.Index)
		if index < 0 || index >= len(vectors) {
			return inference.EmbedResponse{}, inference.NewError(
				inference.InvalidProviderResponse,
				inference.FieldVectors,
				fmt.Errorf("openai: embedding index %d is outside the %d documents sent", index, len(request.Input)),
			)
		}
		vector := make([]float32, len(item.Embedding))
		for i, value := range item.Embedding {
			vector[i] = float32(value)
		}
		vectors[index] = vector
	}

	return inference.EmbedResponse{
		Vectors: vectors,
		Usage:   usageFromEmbedding(raw),
		Metadata: inference.Metadata{
			Model: model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
		},
	}, nil
}

// usageFromEmbedding maps the token counters the endpoint reports.
func usageFromEmbedding(raw openai.CreateEmbeddingResponse) usage.Usage {
	return usage.Usage{
		InputTokens: raw.Usage.PromptTokens,
		TotalTokens: raw.Usage.TotalTokens,
	}
}
