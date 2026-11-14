package dashscope

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

const (
	embedPathText        = "/api/v1/services/embeddings/text-embedding/text-embedding"
	defaultEmbedBatchMax = 10
)

type embedWire struct {
	model     string
	batches   [][]string
	dimension *int
	textType  string
	instruct  string
}

type embedEnvelope struct {
	Output struct {
		Embeddings []struct {
			Embedding []float32 `json:"embedding"`
			TextIndex int       `json:"text_index"`
		} `json:"embeddings"`
	} `json:"output"`
	Usage struct {
		PromptTokens int64 `json:"prompt_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type embedRaw struct {
	vectors     [][]float32
	inputTokens int64
	requestID   string
}

func compileEmbed(declared ModelSpec, name string) protocol.Compiler[inference.EmbedRequest, embedWire] {
	params := declared.Embed
	return func(_ context.Context, request inference.EmbedRequest, _ protocol.Shape) (protocol.Compiled[embedWire], error) {
		wire := embedWire{model: wireModel(name, declared)}
		var warnings []inference.Warning
		for index, item := range request.Input {
			var text strings.Builder
			for _, part := range item.Parts {
				value, ok := part.(message.TextPart)
				if !ok {
					return protocol.Compiled[embedWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldInput,
						fmt.Errorf("dashscope: document %d carries %s, the text-embedding surface accepts text", index, part.Kind()))
				}
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(value.Text)
			}
			wire.batches = append(wire.batches, nil)
			_ = warnings
			wire.batches[len(wire.batches)-1] = append(wire.batches[len(wire.batches)-1], text.String())
		}
		if request.Dimensions > 0 {
			if len(params.Dimensions) > 0 && !slices.Contains(params.Dimensions, request.Dimensions) {
				return protocol.Compiled[embedWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldDimensions,
					fmt.Errorf("dashscope: model accepts dimensions %v, not %d", params.Dimensions, request.Dimensions))
			}
			dimension := request.Dimensions
			wire.dimension = &dimension
		}
		options, err := embedOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[embedWire]{}, err
		}
		if options != nil {
			wire.textType = options.TextType
			if options.Instruct != "" {
				if !params.Instruct {
					return protocol.Compiled[embedWire]{}, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: model does not accept instruct"))
				}
				if wire.textType != "query" {
					return protocol.Compiled[embedWire]{}, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: instruct applies to query embeddings"))
				}
				wire.instruct = options.Instruct
			}
		}
		batchMax := params.BatchMax
		if batchMax < 1 {
			batchMax = defaultEmbedBatchMax
		}
		texts := make([]string, 0)
		for _, batch := range wire.batches {
			texts = append(texts, batch...)
		}
		wire.batches = nil
		for start := 0; start < len(texts); start += batchMax {
			end := min(start+batchMax, len(texts))
			wire.batches = append(wire.batches, append([]string(nil), texts[start:end]...))
		}
		return protocol.Compiled[embedWire]{Wire: wire, Fields: request.ActiveFields(), Warnings: warnings}, nil
	}
}

func embedOptions(extensions inference.Extensions) (*EmbedOptions, error) {
	var found *EmbedOptions
	for _, extension := range extensions {
		options, ok := extension.(*EmbedOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: extension %s/%s is not understood", extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: duplicate embed options"))
		}
		found = options
	}
	if found != nil {
		if err := found.Validate(); err != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, err)
		}
	}
	return found, nil
}

// embedBody is the text-embedding request. The parameters block is omitted
// entirely when nothing sets it, and carries only the knobs the model serves.
type embedBody struct {
	Model      string           `json:"model"`
	Input      embedBodyInput   `json:"input"`
	Parameters *embedBodyParams `json:"parameters,omitempty"`
}

type embedBodyInput struct {
	Texts []string `json:"texts"`
}

type embedBodyParams struct {
	Dimension *int   `json:"dimension,omitempty"`
	TextType  string `json:"text_type,omitempty"`
	Instruct  string `json:"instruct,omitempty"`
}

func embedRequest(wire embedWire, batch []string) embedBody {
	body := embedBody{Model: wire.model, Input: embedBodyInput{Texts: batch}}
	params := embedBodyParams{
		Dimension: wire.dimension, TextType: wire.textType, Instruct: wire.instruct,
	}
	if params != (embedBodyParams{}) {
		body.Parameters = &params
	}
	return body
}

func transportEmbed(client *mediaClient) protocol.Transport[inference.EmbedRequest, embedWire, embedRaw] {
	return func(ctx context.Context, _ inference.EmbedRequest, wire embedWire) (embedRaw, error) {
		raw := embedRaw{}
		for _, batch := range wire.batches {
			var envelope embedEnvelope
			if err := client.postJSON(ctx, embedPathText, false, embedRequest(wire, batch), &envelope); err != nil {
				return embedRaw{}, err
			}
			if envelope.Code != "" {
				return embedRaw{}, errorBody{Code: envelope.Code, Message: envelope.Message, RequestID: envelope.RequestID}.failure("embed", 200)
			}
			start := len(raw.vectors)
			raw.vectors = append(raw.vectors, make([][]float32, len(batch))...)
			for _, item := range envelope.Output.Embeddings {
				if item.TextIndex < 0 || item.TextIndex >= len(batch) {
					return embedRaw{}, fmt.Errorf("dashscope: embedding text_index %d out of range", item.TextIndex)
				}
				raw.vectors[start+item.TextIndex] = item.Embedding
			}
			raw.inputTokens += envelope.Usage.TotalTokens
			if raw.inputTokens == 0 {
				raw.inputTokens += envelope.Usage.PromptTokens
			}
			if raw.requestID == "" {
				raw.requestID = envelope.RequestID
			}
		}
		return raw, nil
	}
}

func decodeEmbed(_ context.Context, request inference.EmbedRequest, raw embedRaw) (inference.EmbedResponse, error) {
	for index, vector := range raw.vectors {
		if len(vector) == 0 {
			return inference.EmbedResponse{}, fmt.Errorf("dashscope: embedding returned no vector for text %d", index)
		}
	}
	return inference.EmbedResponse{
		Vectors:  raw.vectors,
		Usage:    usage.Usage{InputTokens: raw.inputTokens},
		Metadata: inference.Metadata{Model: request.Model.ID, RequestID: raw.requestID},
	}, nil
}

func (d *driver) compileEmbed(ctx context.Context, request inference.EmbedRequest, shape protocol.Shape) (protocol.Compiled[embedWire], error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[embedWire]{}, err
	}
	return compileEmbed(declared, request.Model.ID.Name)(ctx, request, shape)
}
