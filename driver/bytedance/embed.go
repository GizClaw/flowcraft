package bytedance

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

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
	descriptor model.ModelDescriptor,
) protocol.Compiler[inference.EmbedRequest, *embedRequest] {
	return func(
		_ context.Context,
		request inference.EmbedRequest,
		_ protocol.Shape,
	) (protocol.Compiled[*embedRequest], error) {
		multimodal := slices.Contains(descriptor.Capabilities.Inputs, message.PartImage)
		compiled := &embedRequest{}
		texts := make([]string, 0, len(request.Input))
		for _, item := range request.Input {
			var text strings.Builder
			textParts := 0
			inputs := make([]arkmodel.MultimodalEmbeddingInput, 0, len(item.Parts))
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
			for _, part := range item.Parts {
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
						return protocol.Compiled[*embedRequest]{}, inference.NewError(
							inference.UnsupportedFeature, inference.FieldInput,
							fmt.Errorf("bytedance: model embeds text only"),
						)
					}
					flushText()
					url := sourceURI(value.Source)
					inputs = append(inputs, arkmodel.MultimodalEmbeddingInput{
						Type:     arkmodel.MultiModalEmbeddingInputTypeImageURL,
						ImageURL: &arkmodel.MultimodalEmbeddingImageURL{URL: url},
					})
				default:
					return protocol.Compiled[*embedRequest]{}, inference.NewError(
						inference.UnsupportedFeature, inference.FieldInput,
						fmt.Errorf("bytedance: %s parts cannot be embedded", part.Kind()),
					)
				}
			}
			flushText()
			if len(inputs) == 0 {
				continue
			}
			if multimodal {
				fused := arkmodel.MultiModalEmbeddingRequest{Model: endpoint, Input: inputs}
				if request.Dimensions > 0 {
					dimensions := request.Dimensions
					fused.Dimensions = &dimensions
				}
				compiled.multimodal = append(compiled.multimodal, fused)
				continue
			}
			if textParts > 1 || len(inputs) > 1 {
				return protocol.Compiled[*embedRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldInput,
					fmt.Errorf("bytedance: text embedding accepts one text part per item"),
				)
			}
			texts = append(texts, *inputs[0].Text)
		}
		if !multimodal {
			compiled.text = &arkmodel.EmbeddingRequestStrings{
				Input:          texts,
				Model:          endpoint,
				EncodingFormat: arkmodel.EmbeddingEncodingFormatFloat,
			}
			if request.Dimensions > 0 {
				compiled.text.Dimensions = request.Dimensions
			}
		}
		embedded := len(compiled.multimodal)
		if compiled.text != nil {
			embedded = len(compiled.text.Input)
		}
		if embedded != len(request.Input) {
			return protocol.Compiled[*embedRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldInput,
				fmt.Errorf("bytedance: embedding item lost during compile"),
			)
		}
		return protocol.Compiled[*embedRequest]{Wire: compiled, Fields: request.ActiveFields()}, nil
	}
}

func transportEmbed(
	client *arkruntime.Client,
	options []arkruntime.RequestOption,
) protocol.Transport[inference.EmbedRequest, *embedRequest, embedRaw] {
	return func(ctx context.Context, _ inference.EmbedRequest, request *embedRequest) (embedRaw, error) {
		if request.text != nil {
			return transportEmbedText(ctx, client, *request.text, options)
		}
		return transportEmbedMultimodal(ctx, client, request.multimodal, options)
	}
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
	vectors := make([][]float32, len(response.Data))
	for _, item := range response.Data {
		if item.Index < 0 || item.Index >= len(vectors) {
			return embedRaw{}, fmt.Errorf("bytedance: embedding index %d out of range", item.Index)
		}
		vectors[item.Index] = item.Embedding
	}
	return embedRaw{vectors: vectors, inputTokens: int64(response.Usage.TotalTokens)}, nil
}

func transportEmbedMultimodal(
	ctx context.Context,
	client *arkruntime.Client,
	requests []arkmodel.MultiModalEmbeddingRequest,
	options []arkruntime.RequestOption,
) (embedRaw, error) {
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
	request inference.EmbedRequest,
	raw embedRaw,
) (inference.EmbedResponse, error) {
	for index, vector := range raw.vectors {
		if len(vector) == 0 {
			return inference.EmbedResponse{}, fmt.Errorf("bytedance: embedding %d is empty", index)
		}
	}
	return inference.EmbedResponse{
		Vectors:  raw.vectors,
		Usage:    usage.Usage{InputTokens: raw.inputTokens},
		Metadata: inference.Metadata{Model: request.Model.ID},
	}, nil
}

func (d *driver) embed(
	ctx context.Context,
	request inference.EmbedRequest,
	wire *embedRequest,
) (embedRaw, error) {
	return transportEmbed(d.clients.ark, d.clients.arkRequestOptions)(ctx, request, wire)
}

func (d *driver) compileEmbed(
	ctx context.Context,
	request inference.EmbedRequest,
	shape protocol.Shape,
) (protocol.Compiled[*embedRequest], error) {
	declared, err := d.descriptor(request.Model.ID)
	if err != nil {
		return protocol.Compiled[*embedRequest]{}, err
	}
	return compileEmbed(d.clients.endpoint(request.Model.ID.Name), declared)(ctx, request, shape)
}

func (d *driver) bindEmbedder() (protocol.EmbedderDriver, error) {
	return protocol.BindEmbedder(ProviderID, d.compileEmbed, d.embed, decodeEmbed)
}
