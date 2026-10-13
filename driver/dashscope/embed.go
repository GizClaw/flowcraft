package dashscope

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// The text-embedding surface is one synchronous call:
//
//	POST {base}/api/v1/services/embeddings/text-embedding/text-embedding
//
// whose input.texts carries a batch of strings and whose
// output.embeddings[].text_index ties every vector back to its slot in that
// batch. The platform counts tokens once per request (usage.total_tokens, or
// usage.prompt_tokens on the models that report it), so a canonical call that
// needs more than one request reports the sum.
//
// The platform's batch cap is per model version — 10 for text-embedding-v3/
// v4, 20 for qwen3.7-text-embedding, 25 for v1/v2 — so the compiler splits
// the item list into requests the transport issues in order.
const embedPathText = "/api/v1/services/embeddings/text-embedding/text-embedding"

// defaultEmbedBatchMax is the batch cap every model version accepts. A
// declaration raises it where the model allows more.
const defaultEmbedBatchMax = 10

// embedWire is one compiled embedding call: the batches to issue in order and
// the parameters every batch shares.
type embedWire struct {
	model     string
	batches   [][]string
	dimension *int
	textType  string
	instruct  string
}

// embedEnvelope is the reply envelope. Like the generation surfaces, the
// platform reports refusals in band under an HTTP 200.
type embedEnvelope struct {
	Output struct {
		Embeddings []embedItemRaw `json:"embeddings"`
	} `json:"output"`
	Usage     embedUsage `json:"usage"`
	RequestID string     `json:"request_id"`
	Code      string     `json:"code"`
	Message   string     `json:"message"`
}

type embedItemRaw struct {
	Embedding []float32 `json:"embedding"`
	TextIndex int       `json:"text_index"`
}

// embedUsage is the platform's per-request accounting: the batch total on the
// models that report it, the prompt count otherwise.
type embedUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

func (u embedUsage) tokens() int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens
}

// embedRaw is one executed embedding call: the vectors in item order, the
// summed input tokens, and the first request id for correlation.
type embedRaw struct {
	vectors     [][]float32
	inputTokens int64
	requestID   string
}

// compileEmbed lowers a canonical embedding request onto the text-embedding
// surface. Items lower to one string each; the split into batches is the
// only structure the surface adds.
func compileEmbed(
	declared ModelSpec,
	name string,
) inference.Compiler[inference.EmbedRequest, embedWire] {
	params := declared.Embed
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
		wire := embedWire{model: wireModel(name, declared)}
		texts := make([]string, 0, len(request.Items))
		for _, item := range request.Items {
			parts := item.Content.Parts
			var text strings.Builder
			for _, part := range parts {
				value, ok := part.(message.TextPart)
				if !ok {
					l := part.Kind()
					if field, known := inference.EmbedItemPartField(l); known {
						ledger.Reject(
							field,
							fmt.Sprintf(
								"the text-embedding surface embeds text; %s input belongs to an embedding model that declares it",
								l,
							),
						)
					} else {
						// Reasoning has no embedding row: it is not an
						// input at all, so the rejection lands on the item
						// list the request did activate.
						ledger.Reject(
							inference.FieldEmbedItems,
							"reasoning content is not an embedding input",
						)
					}
					continue
				}
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(value.Text)
			}
			if len(parts) > 1 {
				ledger.Drop(
					inference.FieldEmbedItemMultiPart,
					"the platform embeds one string per item; the item's text parts are joined in order",
				)
			}
			texts = append(texts, text.String())
		}
		if request.Dimensions != nil {
			switch {
			case !declared.Capabilities.CustomEmbedDimensions:
				ledger.Reject(
					inference.FieldEmbedDimensions,
					"model does not declare custom embed dimensions",
				)
			case len(params.Dimensions) > 0 &&
				!slices.Contains(params.Dimensions, *request.Dimensions):
				ledger.Reject(
					inference.FieldEmbedDimensions,
					fmt.Sprintf(
						"the model accepts dimensions %s, not %d",
						dimensionList(params.Dimensions), *request.Dimensions,
					),
				)
			default:
				dimension := *request.Dimensions
				wire.dimension = &dimension
			}
		}

		options, other := inference.ExtensionFor[EmbedOptions](request.Extensions)
		if options.TextType != "" {
			wire.textType = options.TextType
		}
		if options.Instruct != "" {
			field := inference.ExtensionField("instruct").Qualify(options)
			switch {
			case !params.Instruct:
				ledger.Reject(
					field,
					"model does not declare the retrieval instruction",
				)
			case wire.textType != "query":
				ledger.Reject(
					field,
					"the retrieval instruction applies to query embeddings; set text_type to \"query\"",
				)
			default:
				wire.instruct = options.Instruct
			}
		}
		ledger.RejectExtensions("text embeddings", other)

		batchMax := params.BatchMax
		if batchMax < 1 {
			batchMax = defaultEmbedBatchMax
		}
		for start := 0; start < len(texts); start += batchMax {
			end := min(start+batchMax, len(texts))
			wire.batches = append(
				wire.batches, append([]string(nil), texts[start:end]...))
		}

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[embedWire]{Report: report}, ledger.Err()
		}
		return inference.Compiled[embedWire]{Wire: wire, Report: report}, nil
	}
}

// embedRequest renders one batch's payload. An empty parameters object is
// omitted: the platform reads an absent parameters block and an empty one the
// same way, and omitting it keeps the batch payload to what it says.
func embedRequest(wire embedWire, batch []string) map[string]any {
	parameters := make(map[string]any)
	if wire.dimension != nil {
		parameters["dimension"] = *wire.dimension
	}
	if wire.textType != "" {
		parameters["text_type"] = wire.textType
	}
	if wire.instruct != "" {
		parameters["instruct"] = wire.instruct
	}
	body := map[string]any{
		"model": wire.model,
		"input": map[string]any{"texts": batch},
	}
	if len(parameters) > 0 {
		body["parameters"] = parameters
	}
	return body
}

// transportEmbed issues the batches in order and assembles the vectors by
// their global item index: text_index is per request, so the transport carries
// the batch offset the wire does not.
func transportEmbed(
	client *mediaClient,
) inference.Transport[embedWire, embedRaw] {
	return func(ctx context.Context, wire embedWire) (embedRaw, error) {
		total := 0
		for _, batch := range wire.batches {
			total += len(batch)
		}
		raw := embedRaw{vectors: make([][]float32, total)}
		offset := 0
		for _, batch := range wire.batches {
			var envelope embedEnvelope
			if err := client.postJSON(
				ctx, embedPathText, false, embedRequest(wire, batch), &envelope,
			); err != nil {
				inference.LogProviderCall(
					ctx, providerID, "embed", wire.model, err, "", "")
				return embedRaw{}, err
			}
			if envelope.Code != "" {
				failure := errorBody{
					Code:      envelope.Code,
					Message:   envelope.Message,
					RequestID: envelope.RequestID,
				}.failure("text embedding", http.StatusOK)
				inference.LogProviderCall(
					ctx, providerID, "embed", wire.model, failure,
					envelope.RequestID, "")
				return embedRaw{}, failure
			}
			for _, item := range envelope.Output.Embeddings {
				if item.TextIndex < 0 || item.TextIndex >= len(batch) {
					failure := fmt.Errorf(
						"dashscope: text embedding returned text_index %d for a batch of %d",
						item.TextIndex, len(batch),
					)
					inference.LogProviderCall(
						ctx, providerID, "embed", wire.model, failure,
						envelope.RequestID, "")
					return embedRaw{}, failure
				}
				raw.vectors[offset+item.TextIndex] = item.Embedding
			}
			raw.inputTokens += envelope.Usage.tokens()
			if raw.requestID == "" {
				raw.requestID = envelope.RequestID
			}
			offset += len(batch)
		}
		inference.LogProviderCall(
			ctx, providerID, "embed", wire.model, nil, raw.requestID, "")
		return raw, nil
	}
}

// decodeEmbed lowers the assembled vectors. A slot the endpoint never filled
// fails the decode: the response validator already promises one vector per
// item, and a zero vector would be a silent lie.
func decodeEmbed(
	_ context.Context,
	raw embedRaw,
) (inference.EmbedResponse, error) {
	embeddings := make([]inference.Embedding, len(raw.vectors))
	for index, vector := range raw.vectors {
		if vector == nil {
			return inference.EmbedResponse{}, fmt.Errorf(
				"dashscope: text embedding returned no vector for text %d",
				index,
			)
		}
		embeddings[index] = inference.Embedding{Vector: vector}
	}
	return inference.EmbedResponse{
		Embeddings: embeddings,
		Usage: inference.EmbedUsage{
			InputTokens: raw.inputTokens,
			ItemCount:   len(embeddings),
		},
		Metadata: inference.Metadata{RequestID: raw.requestID},
	}, nil
}

// openEmbed binds the embedding compiler to the text-embedding surface.
func openEmbed(
	cls *clients,
	declared ModelSpec,
	id model.ModelID,
) (inference.EmbedDriver, error) {
	return inference.BindEmbed(
		compileEmbed(declared, id.Name),
		transportEmbed(cls.media),
		decodeEmbed,
	)
}

// dimensionList renders a declared dimension list for a rejection reason.
func dimensionList(dimensions []int) string {
	rendered := make([]string, 0, len(dimensions))
	for _, dimension := range dimensions {
		rendered = append(rendered, fmt.Sprintf("%d", dimension))
	}
	return strings.Join(rendered, ", ")
}
