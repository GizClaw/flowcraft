package bytedance

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// Embed has two native shapes on Ark: the batched text embeddings endpoint
// and the multimodal endpoint, which fuses one item's text and image inputs
// into a single vector per call. The compiler picks the shape from the
// model's declared capabilities; the wire carries one entry per canonical
// item either way.

type embedWire struct {
	model      string
	multimodal bool
	texts      []string       // text path: one entry per item
	items      [][]embedInput // multimodal path: input list per item
	dimensions *int
}

type embedInput struct {
	kind string // text | image
	text string
	uri  string
}

type embedRaw struct {
	vectors     [][]float32
	inputTokens int64
}

func compileEmbed(
	endpoint string,
	entry catalogEntry,
) inference.Compiler[inference.EmbedRequest, embedWire] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.EmbedRequest,
	) (inference.Compiled[embedWire], error) {
		ledger := inference.NewLedger(
			model.OperationEmbed,
			providerID,
			request.ActiveFields(),
		)
		wire := embedWire{
			model:      endpoint,
			multimodal: slices.Contains(entry.capabilities.Inputs, message.PartImage),
			dimensions: request.Dimensions,
		}
		if request.Dimensions != nil && !entry.capabilities.CustomEmbedDimensions {
			ledger.Reject(
				inference.FieldEmbedDimensions,
				"model does not accept custom dimensions",
			)
		}
		for _, item := range request.Items {
			var text strings.Builder
			textParts := 0
			var inputs []embedInput
			flushText := func() {
				if text.Len() == 0 {
					return
				}
				inputs = append(inputs, embedInput{kind: "text", text: text.String()})
				text.Reset()
			}
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
				case message.ImagePart:
					if !slices.Contains(entry.capabilities.Inputs, message.PartImage) {
						ledger.Reject(
							inference.FieldEmbedItemImage,
							"model embeds text only",
						)
						continue
					}
					flushText()
					inputs = append(inputs, embedInput{
						kind: "image",
						uri:  sourceURI(value.Source),
					})
				case message.AudioPart, message.VideoPart,
					message.FilePart,
					message.ToolCallPart, message.ToolResultPart:
					ledger.Reject(
						embedPartField(part.Kind()),
						fmt.Sprintf("%s parts cannot be embedded", part.Kind()),
					)
				}
			}
			flushText()
			if len(inputs) == 0 {
				continue
			}
			if wire.multimodal {
				wire.items = append(wire.items, inputs)
				continue
			}
			// The text endpoint embeds one string per item; a multi-part item
			// cannot be represented without silently concatenating parts.
			if textParts > 1 || len(inputs) > 1 {
				ledger.Reject(
					inference.FieldEmbedItemMultiPart,
					"text embedding accepts one text part per item",
				)
				continue
			}
			if inputs[0].kind != "text" {
				ledger.Reject(
					inference.FieldEmbedItemImage,
					"model embeds text only",
				)
				continue
			}
			wire.texts = append(wire.texts, inputs[0].text)
		}
		for _, field := range request.Extensions.ActiveFields() {
			ledger.Reject(field, "bytedance embed supports no extensions")
		}

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[embedWire]{Report: report}, ledger.Err()
		}
		if wire.multimodal && len(wire.items) != len(request.Items) {
			// Cannot happen without a rejection above; guard the invariant.
			return inference.Compiled[embedWire]{Report: report}, inference.NewError(
				inference.UnsupportedFeature,
				model.OperationEmbed,
				inference.FieldEmbedItems,
				fmt.Errorf("bytedance: embedding item lost during compile"),
			)
		}
		return inference.Compiled[embedWire]{Wire: wire, Report: report}, nil
	}
}

func transportEmbed(
	client *arkruntime.Client,
	options []arkruntime.RequestOption,
) inference.Transport[embedWire, embedRaw] {
	return func(ctx context.Context, wire embedWire) (embedRaw, error) {
		var raw embedRaw
		var err error
		if wire.multimodal {
			raw, err = transportEmbedMultimodal(ctx, client, wire, options)
		} else {
			raw, err = transportEmbedText(ctx, client, wire, options)
		}
		if err != nil {
			inference.LogProviderCall(ctx, providerID, "embed", wire.model, err, "", "")
			return raw, err
		}
		inference.LogProviderCall(ctx, providerID, "embed", wire.model, nil, "", "")
		return raw, nil
	}
}

func transportEmbedText(
	ctx context.Context,
	client *arkruntime.Client,
	wire embedWire,
	options []arkruntime.RequestOption,
) (embedRaw, error) {
	request := arkmodel.EmbeddingRequestStrings{
		Input:          wire.texts,
		Model:          wire.model,
		EncodingFormat: arkmodel.EmbeddingEncodingFormatFloat,
	}
	if wire.dimensions != nil {
		request.Dimensions = *wire.dimensions
	}
	response, err := client.CreateEmbeddings(ctx, request, options...)
	if err != nil {
		return embedRaw{}, classifyError(err)
	}
	raw := embedRaw{inputTokens: int64(response.Usage.TotalTokens)}
	vectors := make([][]float32, len(response.Data))
	for _, item := range response.Data {
		if item.Index < 0 || item.Index >= len(vectors) {
			return embedRaw{}, fmt.Errorf(
				"bytedance: embedding index %d out of range",
				item.Index,
			)
		}
		vectors[item.Index] = item.Embedding
	}
	raw.vectors = vectors
	return raw, nil
}

func transportEmbedMultimodal(
	ctx context.Context,
	client *arkruntime.Client,
	wire embedWire,
	options []arkruntime.RequestOption,
) (embedRaw, error) {
	// The multimodal endpoint fuses one item's inputs into a single vector
	// per call, so items are embedded one request at a time.
	raw := embedRaw{vectors: make([][]float32, 0, len(wire.items))}
	for _, inputs := range wire.items {
		request := arkmodel.MultiModalEmbeddingRequest{
			Model:      wire.model,
			Dimensions: wire.dimensions,
		}
		for _, input := range inputs {
			entry := arkmodel.MultimodalEmbeddingInput{}
			switch input.kind {
			case "text":
				entry.Type = arkmodel.MultiModalEmbeddingInputTypeText
				entry.Text = &input.text
			case "image":
				entry.Type = arkmodel.MultiModalEmbeddingInputTypeImageURL
				entry.ImageURL = &arkmodel.MultimodalEmbeddingImageURL{URL: input.uri}
			}
			request.Input = append(request.Input, entry)
		}
		response, err := client.CreateMultiModalEmbeddings(ctx, request, options...)
		if err != nil {
			return embedRaw{}, classifyError(err)
		}
		raw.vectors = append(raw.vectors, response.Data.Embedding)
		raw.inputTokens += int64(response.Usage.TotalTokens)
	}
	return raw, nil
}

func decodeEmbed(
	_ context.Context,
	raw embedRaw,
) (inference.EmbedResponse, error) {
	embeddings := make([]inference.Embedding, len(raw.vectors))
	for index, vector := range raw.vectors {
		if len(vector) == 0 {
			return inference.EmbedResponse{}, fmt.Errorf(
				"bytedance: embedding %d is empty",
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
	spec Spec,
	entry catalogEntry,
	id model.ModelID,
	profile string,
) (inference.EmbedDriver, error) {
	ark, err := cls.requireArk(profile)
	if err != nil {
		return nil, err
	}
	return inference.BindEmbed(
		compileEmbed(cls.endpoint(id.Name), entry),
		transportEmbed(ark, cls.arkRequestOptions),
		decodeEmbed,
	)
}
