package bytedance

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/outcome"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/driver/internal/httpx"
)

// The embedding surfaces are OpenAI-shaped JSON; these are the fields the driver
// reads and sends, and nothing else. The vendor SDK's types carried the whole
// catalogue of options for them.
type embedRequest struct {
	text       *embeddingRequest
	multimodal []multimodalEmbeddingRequest
}

type embeddingRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	Dimensions     *int     `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type multimodalEmbeddingInput struct {
	Type     string              `json:"type"`
	ImageURL *multimodalImageURL `json:"image_url,omitempty"`
	Text     string              `json:"text,omitempty"`
}

type multimodalImageURL struct {
	URL string `json:"url"`
}

type multimodalEmbeddingRequest struct {
	Model      string                     `json:"model"`
	Input      []multimodalEmbeddingInput `json:"input"`
	Dimensions *int                       `json:"dimensions,omitempty"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

type multimodalEmbeddingResponse struct {
	Data struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
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
			var (
				text strings.Builder
				// flushed holds the text of this item on the text surface, where
				// it goes into the batch as a string. The fused surface builds
				// items instead, so a text model pays for neither the slice nor
				// the wrapper it would never send.
				flushed   string
				inputs    []multimodalEmbeddingInput
				textParts int
			)
			flushText := func() {
				if text.Len() == 0 {
					return
				}
				value := text.String()
				text.Reset()
				if !multimodal {
					flushed = value
					return
				}
				inputs = append(inputs, multimodalEmbeddingInput{
					Type: "text",
					Text: value,
				})
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
					inputs = append(inputs, multimodalEmbeddingInput{
						Type:     "image_url",
						ImageURL: &multimodalImageURL{URL: url},
					})
				default:
					return protocol.Compiled[*embedRequest]{}, inference.NewError(
						inference.UnsupportedFeature, inference.FieldInput,
						fmt.Errorf("bytedance: %s parts cannot be embedded", part.Kind()),
					)
				}
			}
			flushText()
			if multimodal && len(inputs) == 0 {
				continue
			}
			if multimodal {
				fused := multimodalEmbeddingRequest{Model: endpoint, Input: inputs}
				if request.Dimensions > 0 {
					dimensions := request.Dimensions
					fused.Dimensions = &dimensions
				}
				compiled.multimodal = append(compiled.multimodal, fused)
				continue
			}
			if textParts > 1 {
				return protocol.Compiled[*embedRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldInput,
					fmt.Errorf("bytedance: text embedding accepts one text part per item"),
				)
			}
			texts = append(texts, flushed)
		}
		if !multimodal {
			compiled.text = &embeddingRequest{
				Input:          texts,
				Model:          endpoint,
				EncodingFormat: "float",
			}
			if request.Dimensions > 0 {
				dimensions := request.Dimensions
				compiled.text.Dimensions = &dimensions
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

// embeddingPath and multimodalEmbeddingPath are the two surfaces the platform
// serves embeddings on: one text batch per call on the first, one fused
// text-and-image item per call on the second.
const (
	embeddingPath           = "/embeddings"
	multimodalEmbeddingPath = "/embeddings/multimodal"
)

func transportEmbed(
	client *httpx.Client,
	query url.Values,
) protocol.Transport[inference.EmbedRequest, *embedRequest, embedRaw] {
	return func(ctx context.Context, _ inference.EmbedRequest, request *embedRequest) (embedRaw, error) {
		if request.text != nil {
			return transportEmbedText(ctx, client, query, *request.text)
		}
		return transportEmbedMultimodal(ctx, client, query, request.multimodal)
	}
}

func transportEmbedText(
	ctx context.Context,
	client *httpx.Client,
	query url.Values,
	request embeddingRequest,
) (embedRaw, error) {
	var response embeddingResponse
	if err := client.JSON(ctx, httpx.Op{
		Method: http.MethodPost, Path: embeddingPath, Body: request, Query: query,
	}, &response); err != nil {
		return embedRaw{}, err
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

// transportEmbedMultimodal fuses text and image into one vector per item, so the
// items are embedded one call at a time and the vectors kept in request order.
func transportEmbedMultimodal(
	ctx context.Context,
	client *httpx.Client,
	query url.Values,
	requests []multimodalEmbeddingRequest,
) (embedRaw, error) {
	raw := embedRaw{vectors: make([][]float32, 0, len(requests))}
	for _, request := range requests {
		var response multimodalEmbeddingResponse
		if err := client.JSON(ctx, httpx.Op{
			Method: http.MethodPost, Path: multimodalEmbeddingPath, Body: request, Query: query,
		}, &response); err != nil {
			return embedRaw{}, err
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
		Usage:    outcome.Usage{InputTokens: raw.inputTokens},
		Metadata: outcome.Metadata{Model: request.Model.ID},
	}, nil
}

func (d *driver) embed(
	ctx context.Context,
	request inference.EmbedRequest,
	wire *embedRequest,
) (embedRaw, error) {
	return transportEmbed(d.clients.api, d.clients.query)(ctx, request, wire)
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
