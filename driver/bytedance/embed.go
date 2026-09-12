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

// Embed has two native shapes on Ark: the batched text embeddings endpoint and
// the multimodal endpoint, which fuses one item's text and image inputs into a
// single vector per call. The compiler picks the shape from the model's
// declared capabilities, so exactly one half of embedRequest is set.

// embedRequest is one compiled embeddings call: the text endpoint's batched
// request (text non-nil), or one multimodal request per canonical item (text
// nil). Exactly one half is used, and the shape never changes within a call.
type embedRequest struct {
	text       *arkmodel.EmbeddingRequestStrings
	multimodal []arkmodel.MultiModalEmbeddingRequest
}

type embedRaw struct {
	vectors     [][]float32
	inputTokens int64
}

func compileEmbed(
	endpoint string,
	entry catalogEntry,
) inference.Compiler[inference.EmbedRequest, *embedRequest] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.EmbedRequest,
	) (inference.Compiled[*embedRequest], error) {
		ledger := inference.NewLedger(
			model.OperationEmbed,
			providerID,
			request.ActiveFields(),
		)
		multimodal := slices.Contains(entry.capabilities.Inputs, message.PartImage)
		compiled := &embedRequest{}
		if request.Dimensions != nil {
			if !entry.capabilities.CustomEmbedDimensions {
				ledger.Reject(
					inference.FieldEmbedDimensions,
					"model does not accept custom dimensions",
				)
			}
		}

		texts := make([]string, 0, len(request.Items))
		for _, item := range request.Items {
			var text strings.Builder
			textParts := 0
			inputs := make([]arkmodel.MultimodalEmbeddingInput, 0, len(item.Content.Parts))
			flushText := func() {
				if text.Len() == 0 {
					return
				}
				value := text.String()
				inputs = append(inputs, arkmodel.MultimodalEmbeddingInput{
					Type: arkmodel.MultiModalEmbeddingInputTypeText,
					Text: &value,
				})
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
					if !multimodal {
						ledger.Reject(
							inference.FieldEmbedItemImage,
							"model embeds text only",
						)
						continue
					}
					flushText()
					url := sourceURI(value.Source)
					inputs = append(inputs, arkmodel.MultimodalEmbeddingInput{
						Type: arkmodel.MultiModalEmbeddingInputTypeImageURL,
						ImageURL: &arkmodel.MultimodalEmbeddingImageURL{
							URL: url,
						},
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
			if multimodal {
				fused := arkmodel.MultiModalEmbeddingRequest{
					Model: endpoint,
					Input: inputs,
				}
				if request.Dimensions != nil {
					dimensions := *request.Dimensions
					fused.Dimensions = &dimensions
				}
				compiled.multimodal = append(compiled.multimodal, fused)
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
			texts = append(texts, *inputs[0].Text)
		}
		for _, field := range request.Extensions.ActiveFields() {
			ledger.Reject(field, "bytedance embed supports no extensions")
		}

		if !multimodal {
			compiled.text = &arkmodel.EmbeddingRequestStrings{
				Input:          texts,
				Model:          endpoint,
				EncodingFormat: arkmodel.EmbeddingEncodingFormatFloat,
			}
			if request.Dimensions != nil {
				compiled.text.Dimensions = *request.Dimensions
			}
		}

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[*embedRequest]{Report: report}, ledger.Err()
		}
		embedded := len(compiled.multimodal)
		if compiled.text != nil {
			embedded = len(compiled.text.Input)
		}
		if embedded != len(request.Items) {
			// Cannot happen without a rejection above; guard the invariant.
			return inference.Compiled[*embedRequest]{Report: report}, inference.NewError(
				inference.UnsupportedFeature,
				model.OperationEmbed,
				inference.FieldEmbedItems,
				fmt.Errorf("bytedance: embedding item lost during compile"),
			)
		}
		return inference.Compiled[*embedRequest]{Wire: compiled, Report: report}, nil
	}
}

func transportEmbed(
	client *arkruntime.Client,
	options []arkruntime.RequestOption,
) inference.Transport[*embedRequest, embedRaw] {
	return func(ctx context.Context, request *embedRequest) (embedRaw, error) {
		var raw embedRaw
		var err error
		if request.text != nil {
			raw, err = transportEmbedText(ctx, client, *request.text, options)
		} else {
			raw, err = transportEmbedMultimodal(ctx, client, request.multimodal, options)
		}
		if err != nil {
			inference.LogProviderCall(ctx, providerID, "embed", embeddingModel(request), err, "", "")
			return raw, err
		}
		inference.LogProviderCall(ctx, providerID, "embed", embeddingModel(request), nil, "", "")
		return raw, nil
	}
}

// embeddingModel names the model for telemetry: every item of one compile
// shares the endpoint.
func embeddingModel(request *embedRequest) string {
	if request.text != nil {
		return request.text.Model
	}
	if len(request.multimodal) == 0 {
		return ""
	}
	return request.multimodal[0].Model
}

func transportEmbedText(
	ctx context.Context,
	client *arkruntime.Client,
	request arkmodel.EmbeddingRequestStrings,
	options []arkruntime.RequestOption,
) (embedRaw, error) {
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
	requests []arkmodel.MultiModalEmbeddingRequest,
	options []arkruntime.RequestOption,
) (embedRaw, error) {
	// The multimodal endpoint fuses one item's inputs into a single vector
	// per call, so items are embedded one request at a time.
	raw := embedRaw{vectors: make([][]float32, 0, len(requests))}
	for _, request := range requests {
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
