package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// Embed has one native shape: the batched text embeddings endpoint. The wire
// carries one string per canonical item; text and data parts fuse into that
// string (data lowers to its JSON text), while any other part kind is
// rejected.

type embedWire struct {
	model      string
	texts      []string
	dimensions *int
}

type embedRaw struct {
	vectors     [][]float32
	inputTokens int64
}

var embedPartFields = map[message.PartKind]inference.FieldID{
	message.PartText:       inference.FieldEmbedItemText,
	message.PartImage:      inference.FieldEmbedItemImage,
	message.PartAudio:      inference.FieldEmbedItemAudio,
	message.PartVideo:      inference.FieldEmbedItemVideo,
	message.PartFile:       inference.FieldEmbedItemFile,
	message.PartData:       inference.FieldEmbedItemData,
	message.PartToolCall:   inference.FieldEmbedItemToolCall,
	message.PartToolResult: inference.FieldEmbedItemToolResult,
}

func compileEmbed(
	model string,
	entry catalogEntry,
) inference.Compiler[inference.EmbedRequest, embedWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.EmbedRequest,
	) (inference.Compiled[embedWire], error) {
		ledger := newLedger(inference.OperationEmbed, request.ActiveFields())
		wire := embedWire{
			model:      model,
			dimensions: request.Dimensions,
		}
		if request.Dimensions != nil && !entry.dimensions {
			ledger.reject(
				inference.FieldEmbedDimensions,
				"model does not accept custom dimensions",
			)
		}
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
					ledger.reject(
						embedPartFields[part.Kind()],
						fmt.Sprintf("%s parts cannot be embedded", part.Kind()),
					)
				}
			}
			if text.Len() == 0 && textParts == 0 {
				continue
			}
			if textParts > 1 {
				ledger.reject(
					inference.FieldEmbedItemMultiPart,
					"text embedding accepts one text part per item",
				)
				continue
			}
			wire.texts = append(wire.texts, text.String())
		}
		for _, field := range request.Extensions.ActiveFields() {
			ledger.reject(field, "openai embed supports no extensions")
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[embedWire]{Report: report}, ledger.err()
		}
		if len(wire.texts) != len(request.Items) {
			// Cannot happen without a rejection above; guard the invariant.
			return inference.Compiled[embedWire]{Report: report}, inference.NewError(
				inference.UnsupportedFeature,
				inference.OperationEmbed,
				inference.FieldEmbedItems,
				fmt.Errorf("azure: embedding item lost during compile"),
			)
		}
		return inference.Compiled[embedWire]{Wire: wire, Report: report}, nil
	}
}

func transportEmbed(
	client openai.Client,
) inference.Transport[embedWire, embedRaw] {
	return func(ctx context.Context, wire embedWire) (embedRaw, error) {
		params := openai.EmbeddingNewParams{
			Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: wire.texts},
			Model: wire.model,
		}
		if wire.dimensions != nil {
			params.Dimensions = param.NewOpt(int64(*wire.dimensions))
		}
		response, err := client.Embeddings.New(ctx, params)
		if err != nil {
			classified := classifyError(err)
			logInferenceCall(ctx, "embed", wire.model, classified, "", "")
			return embedRaw{}, classified
		}
		vectors := make([][]float32, len(response.Data))
		for _, item := range response.Data {
			if item.Index < 0 || item.Index >= int64(len(vectors)) {
				return embedRaw{}, fmt.Errorf(
					"azure: embedding index %d out of range",
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
		logInferenceCall(ctx, "embed", wire.model, nil, "", "")
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
				"azure: embedding %d is empty",
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
	id inference.ModelID,
	_ string,
) (inference.EmbedDriver, error) {
	return inference.BindEmbed(
		compileEmbed(id.Name, entry),
		transportEmbed(cls.api),
		decodeEmbed,
	)
}
