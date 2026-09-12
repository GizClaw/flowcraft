package openai

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// Embed has one native shape: the batched text embeddings endpoint. The
// compiled request carries one input string per canonical item; text and data
// parts fuse into that string (data lowers to its JSON text), while any other
// part kind is rejected.

type embedRaw struct {
	vectors     [][]float32
	inputTokens int64
}

var embedPartField = partField(inference.EmbedItemPartField)

func compileEmbed(
	modelName string,
	entry catalogEntry,
) inference.Compiler[inference.EmbedRequest, openai.EmbeddingNewParams] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.EmbedRequest,
	) (inference.Compiled[openai.EmbeddingNewParams], error) {
		ledger := inference.NewLedger(model.OperationEmbed, providerID, request.ActiveFields())
		params := openai.EmbeddingNewParams{Model: modelName}
		if request.Dimensions != nil {
			if !entry.capabilities.CustomEmbedDimensions {
				ledger.Reject(
					inference.FieldEmbedDimensions,
					"model does not accept custom dimensions",
				)
			} else {
				params.Dimensions = param.NewOpt(int64(*request.Dimensions))
			}
		}
		texts := make([]string, 0, len(request.Items))
		for _, item := range request.Items {
			var text strings.Builder
			textParts := 0
			for _, part := range item.Content.Parts {
				switch value := part.(type) {
				case message.TextPart:
					textParts++
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(value.Text)
				case message.DataPart:
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(string(value.Value))
				default:
					ledger.Reject(
						embedPartField(part.Kind()),
						fmt.Sprintf("%s parts cannot be embedded", part.Kind()),
					)
				}
			}
			if text.Len() == 0 && textParts == 0 {
				continue
			}
			if textParts > 1 {
				ledger.Reject(
					inference.FieldEmbedItemMultiPart,
					"text embedding accepts one text part per item",
				)
				continue
			}
			texts = append(texts, text.String())
		}
		params.Input = openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts}
		for _, field := range request.Extensions.ActiveFields() {
			ledger.Reject(field, "openai embed supports no extensions")
		}

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[openai.EmbeddingNewParams]{Report: report}, ledger.Err()
		}
		if len(texts) != len(request.Items) {
			// Cannot happen without a rejection above; guard the invariant.
			return inference.Compiled[openai.EmbeddingNewParams]{Report: report}, inference.NewError(
				inference.UnsupportedFeature,
				model.OperationEmbed,
				inference.FieldEmbedItems,
				fmt.Errorf("openai: embedding item lost during compile"),
			)
		}
		return inference.Compiled[openai.EmbeddingNewParams]{
			Wire:   params,
			Report: report,
		}, nil
	}
}

func transportEmbed(
	client openai.Client,
) inference.Transport[openai.EmbeddingNewParams, embedRaw] {
	return func(
		ctx context.Context,
		params openai.EmbeddingNewParams,
	) (embedRaw, error) {
		response, err := client.Embeddings.New(ctx, params)
		if err != nil {
			classified := classifyError(err)
			inference.LogProviderCall(ctx, providerID, "embed", params.Model, classified, "", "")
			return embedRaw{}, classified
		}
		vectors := make([][]float32, len(response.Data))
		for _, item := range response.Data {
			if item.Index < 0 || item.Index >= int64(len(vectors)) {
				return embedRaw{}, fmt.Errorf(
					"openai: embedding index %d out of range",
					item.Index,
				)
			}
			vector := make([]float32, len(item.Embedding))
			for index, value := range item.Embedding {
				vector[index] = float32(value)
			}
			vectors[item.Index] = vector
		}
		raw := embedRaw{
			vectors:     vectors,
			inputTokens: response.Usage.TotalTokens,
		}
		inference.LogProviderCall(ctx, providerID, "embed", params.Model, nil, "", "")
		return raw, nil
	}
}

func decodeEmbed(
	_ context.Context,
	raw embedRaw,
) (inference.EmbedResponse, error) {
	embeddings := make([]inference.Embedding, len(raw.vectors))
	for index, vector := range raw.vectors {
		if len(vector) == 0 {
			return inference.EmbedResponse{}, fmt.Errorf(
				"openai: embedding %d is empty",
				index,
			)
		}
		embeddings[index] = inference.Embedding{Vector: vector}
	}
	return inference.EmbedResponse{
		Embeddings: embeddings,
		Usage: inference.EmbedUsage{
			InputTokens: raw.inputTokens,
			ItemCount:   len(raw.vectors),
		},
	}, nil
}

func openEmbed(
	cls *clients,
	entry catalogEntry,
	id model.ModelID,
	_ string,
) (inference.EmbedDriver, error) {
	return inference.BindEmbed(
		compileEmbed(id.Name, entry),
		transportEmbed(cls.api),
		decodeEmbed,
	)
}
