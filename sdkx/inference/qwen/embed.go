package qwen

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// Embedding endpoint paths under the API root.
const (
	pathTextEmbedding       = "/api/v1/services/embeddings/text-embedding/text-embedding"
	pathMultimodalEmbedding = "/api/v1/services/embeddings/multimodal-embedding/multimodal-embedding"
)

// Batch ceilings from the model overview: text-embedding takes at most 10
// rows per request; multimodal-embedding takes at most 20 content elements
// and 5 images per request.
const (
	maxTextEmbedRows      = 10
	maxMultimodalContents = 20
	maxMultimodalImages   = 5
)

// embedShape picks the wire/transport dialect.
type embedShape int

const (
	// embedShapeText is the text-embedding batch: one string per item.
	embedShapeText embedShape = iota
	// embedShapeIndependent is the multimodal independent-vector batch:
	// one content per canonical item, every item single-part.
	embedShapeIndependent
	// embedShapeFusion fuses each multi-part item into one vector through
	// enable_fusion, one request per item.
	embedShapeFusion
)

// embedWire is the compiled embedding request, fully concrete per the
// runtime's wire contract; the transport renders the native body per
// shape.
type embedWire struct {
	Path      string           `json:"-"`
	Model     string           `json:"-"`
	Shape     embedShape       `json:"-"`
	Texts     []string         `json:"-"`
	Contents  []embedContent   `json:"-"` // independent: one per canonical item
	Items     [][]embedContent `json:"-"` // fusion: one entry per canonical item
	Dimension *int             `json:"-"`
	TextType  string           `json:"-"`
	Instruct  string           `json:"-"`
}

// embedContent is one multimodal-embedding contents element; exactly one
// field is set.
type embedContent struct {
	Text  *string `json:"text,omitempty"`
	Image string  `json:"image,omitempty"`
	Video string  `json:"video,omitempty"`
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
		options, other := operationExtensions[EmbedOptions](request.Extensions)
		rejectOtherExtensions("embed", other, ledger)

		wire := embedWire{Model: model}
		if entry.multimodal() {
			wire.Path = pathMultimodalEmbedding
		} else {
			wire.Path = pathTextEmbedding
		}

		if request.Dimensions != nil {
			if !slices.Contains(entry.embedDimensions, *request.Dimensions) {
				ledger.reject(
					inference.FieldEmbedDimensions,
					fmt.Sprintf("model %s dimensions must be one of %v", model, entry.embedDimensions),
				)
			} else {
				dimension := *request.Dimensions
				wire.Dimension = &dimension
			}
		}
		if options.TextType != "" {
			if entry.multimodal() {
				ledger.reject(
					inference.ExtensionField("text_type").Qualify(options),
					"text_type exists on text-embedding only",
				)
			} else {
				wire.TextType = options.TextType
			}
		}
		wire.Instruct = options.Instruct

		if entry.multimodal() {
			compileEmbedMultimodal(&wire, entry, request, ledger)
		} else {
			compileEmbedText(&wire, request, ledger)
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[embedWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[embedWire]{Wire: wire, Report: report}, nil
	}
}

// compileEmbedText compiles the text-embedding batch: exactly one text
// part per item, at most maxTextEmbedRows rows.
func compileEmbedText(
	wire *embedWire,
	request inference.EmbedRequest,
	ledger *ledger,
) {
	if len(request.Items) > maxTextEmbedRows {
		ledger.reject(
			inference.FieldEmbedItems,
			fmt.Sprintf("text-embedding batches at most %d rows per request", maxTextEmbedRows),
		)
		return
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
					"model embeds text only",
				)
			}
		}
		if textParts > 1 {
			ledger.reject(
				inference.FieldEmbedItemMultiPart,
				"text embedding accepts one text part per item",
			)
			continue
		}
		if text.Len() == 0 && textParts == 0 {
			continue
		}
		wire.Texts = append(wire.Texts, text.String())
	}
}

// compileEmbedMultimodal compiles qwen3-vl-embedding requests. Items that
// are all single-part batch into one independent-vector call; any
// multi-part item switches the whole compile to per-item fusion calls.
func compileEmbedMultimodal(
	wire *embedWire,
	_ catalogEntry,
	request inference.EmbedRequest,
	ledger *ledger,
) {
	items := make([][]embedContent, 0, len(request.Items))
	multiPart := false
	for _, item := range request.Items {
		contents := make([]embedContent, 0, len(item.Content.Parts))
		for _, part := range item.Content.Parts {
			switch typed := part.(type) {
			case message.TextPart:
				text := typed.Text
				contents = append(contents, embedContent{Text: &text})
			case message.ImagePart:
				contents = append(contents, embedContent{Image: embedImageValue(typed.Source, ledger)})
			case message.VideoPart:
				value, ok := videoValue(typed.Source)
				if !ok {
					ledger.reject(
						inference.FieldEmbedItemVideo,
						"video input must be a URL; inline bytes are unsupported",
					)
					continue
				}
				contents = append(contents, embedContent{Video: value})
			case message.DataPart:
				data := "\n" + string(typed.Value) + "\n"
				contents = append(contents, embedContent{Text: &data})
			default:
				ledger.reject(
					embedPartFields[part.Kind()],
					fmt.Sprintf("%s parts cannot be embedded", part.Kind()),
				)
			}
		}
		if len(contents) == 0 {
			continue
		}
		if len(contents) > 1 {
			multiPart = true
		}
		if err := multimodalContentsWithinLimits(contents); err != nil {
			ledger.reject(inference.FieldEmbedItems, err.Error())
			continue
		}
		items = append(items, contents)
	}

	if !multiPart {
		wire.Shape = embedShapeIndependent
		for _, contents := range items {
			wire.Contents = append(wire.Contents, contents[0])
		}
		if err := multimodalContentsWithinLimits(wire.Contents); err != nil {
			ledger.reject(inference.FieldEmbedItems, err.Error())
		}
		return
	}
	wire.Shape = embedShapeFusion
	wire.Items = items
}

// multimodalContentsWithinLimits enforces the per-request ceilings: at
// most 20 content elements and 5 images, shared across modalities.
func multimodalContentsWithinLimits(contents []embedContent) error {
	if len(contents) > maxMultimodalContents {
		return fmt.Errorf("multimodal embedding takes at most %d content elements per request", maxMultimodalContents)
	}
	images := 0
	for _, content := range contents {
		if content.Image != "" {
			images++
		}
	}
	if images > maxMultimodalImages {
		return fmt.Errorf("multimodal embedding takes at most %d images per request", maxMultimodalImages)
	}
	return nil
}

// embedImageValue renders one image content: URL or Base64 data URI.
func embedImageValue(source media.ImageSource, ledger *ledger) string {
	if source.Kind() != media.SourceURL && source.Kind() != media.SourceInline {
		ledger.reject(inference.FieldEmbedItemImage, "image source must be a URL or inline bytes")
		return ""
	}
	return imageValue(source)
}

// embedRaw is the transport result: vectors aligned to canonical item
// order plus the aggregated input tokens.
type embedRaw struct {
	vectors     [][]float32
	inputTokens int64
	requestID   string
}

// dashEmbedEnvelope is the embedding response envelope, shared by the
// text and multimodal endpoints (the text endpoint indexes by text_index,
// the multimodal one by index).
type dashEmbedEnvelope struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Output    struct {
		Embeddings []struct {
			Index     int       `json:"index"`
			TextIndex int       `json:"text_index"`
			Embedding []float32 `json:"embedding"`
		} `json:"embeddings"`
	} `json:"output"`
	Usage struct {
		InputTokens int64 `json:"input_tokens"`
		ImageTokens int64 `json:"image_tokens"`
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage"`
}

func (e dashEmbedEnvelope) tokens() int64 {
	if e.Usage.TotalTokens > 0 {
		return e.Usage.TotalTokens
	}
	return e.Usage.InputTokens + e.Usage.ImageTokens
}

func transportEmbed(
	client *dashClient,
) inference.Transport[embedWire, embedRaw] {
	return func(ctx context.Context, wire embedWire) (embedRaw, error) {
		switch wire.Shape {
		case embedShapeText:
			return transportEmbedText(ctx, client, wire)
		case embedShapeIndependent:
			return transportEmbedIndependent(ctx, client, wire)
		case embedShapeFusion:
			return transportEmbedFusion(ctx, client, wire)
		}
		return embedRaw{}, fmt.Errorf("qwen: unknown embed shape %d", wire.Shape)
	}
}

func transportEmbedText(
	ctx context.Context,
	client *dashClient,
	wire embedWire,
) (embedRaw, error) {
	body := struct {
		Model string `json:"model"`
		Input struct {
			Texts []string `json:"texts"`
		} `json:"input"`
		Parameters struct {
			Dimension *int   `json:"dimension,omitempty"`
			TextType  string `json:"text_type,omitempty"`
			Instruct  string `json:"instruct,omitempty"`
		} `json:"parameters"`
	}{Model: wire.Model}
	body.Input.Texts = wire.Texts
	body.Parameters.Dimension = wire.Dimension
	body.Parameters.TextType = wire.TextType
	body.Parameters.Instruct = wire.Instruct

	var envelope dashEmbedEnvelope
	if err := client.postJSON(ctx, wire.Path, body, &envelope); err != nil {
		return embedRaw{}, err
	}
	if err := classifyEnvelope(
		envelope.Code, envelope.Message, envelope.RequestID,
	); err != nil {
		return embedRaw{}, err
	}
	vectors := make([][]float32, len(wire.Texts))
	for _, embedding := range envelope.Output.Embeddings {
		if embedding.TextIndex < 0 || embedding.TextIndex >= len(vectors) {
			return embedRaw{}, fmt.Errorf(
				"qwen: embedding text_index %d out of range",
				embedding.TextIndex,
			)
		}
		vectors[embedding.TextIndex] = embedding.Embedding
	}
	return embedRaw{
		vectors:     vectors,
		inputTokens: envelope.tokens(),
		requestID:   envelope.RequestID,
	}, nil
}

// multimodalEmbedBody is the multimodal-embedding request body, used by
// both the independent batch and per-item fusion calls.
type multimodalEmbedBody struct {
	Model string `json:"model"`
	Input struct {
		Contents []embedContent `json:"contents"`
	} `json:"input"`
	Parameters struct {
		Dimension    *int   `json:"dimension,omitempty"`
		Instruct     string `json:"instruct,omitempty"`
		EnableFusion bool   `json:"enable_fusion,omitempty"`
	} `json:"parameters"`
}

func newMultimodalBody(
	wire embedWire,
	contents []embedContent,
	fusion bool,
) multimodalEmbedBody {
	var body multimodalEmbedBody
	body.Model = wire.Model
	body.Input.Contents = contents
	body.Parameters.Dimension = wire.Dimension
	body.Parameters.Instruct = wire.Instruct
	body.Parameters.EnableFusion = fusion
	return body
}

func transportEmbedIndependent(
	ctx context.Context,
	client *dashClient,
	wire embedWire,
) (embedRaw, error) {
	var envelope dashEmbedEnvelope
	if err := client.postJSON(
		ctx, wire.Path, newMultimodalBody(wire, wire.Contents, false), &envelope,
	); err != nil {
		return embedRaw{}, err
	}
	if err := classifyEnvelope(
		envelope.Code, envelope.Message, envelope.RequestID,
	); err != nil {
		return embedRaw{}, err
	}
	vectors := make([][]float32, len(wire.Contents))
	for _, embedding := range envelope.Output.Embeddings {
		if embedding.Index < 0 || embedding.Index >= len(vectors) {
			return embedRaw{}, fmt.Errorf(
				"qwen: embedding index %d out of range",
				embedding.Index,
			)
		}
		vectors[embedding.Index] = embedding.Embedding
	}
	return embedRaw{
		vectors:     vectors,
		inputTokens: envelope.tokens(),
		requestID:   envelope.RequestID,
	}, nil
}

// transportEmbedFusion fuses each multi-part item into one vector, one
// request per item, and aggregates the usage across calls.
func transportEmbedFusion(
	ctx context.Context,
	client *dashClient,
	wire embedWire,
) (embedRaw, error) {
	raw := embedRaw{vectors: make([][]float32, 0, len(wire.Items))}
	for _, contents := range wire.Items {
		var envelope dashEmbedEnvelope
		if err := client.postJSON(
			ctx, wire.Path, newMultimodalBody(wire, contents, true), &envelope,
		); err != nil {
			return embedRaw{}, err
		}
		if err := classifyEnvelope(
			envelope.Code, envelope.Message, envelope.RequestID,
		); err != nil {
			return embedRaw{}, err
		}
		if len(envelope.Output.Embeddings) != 1 {
			return embedRaw{}, fmt.Errorf(
				"qwen: fusion returned %d embeddings, want 1",
				len(envelope.Output.Embeddings),
			)
		}
		raw.vectors = append(raw.vectors, envelope.Output.Embeddings[0].Embedding)
		raw.inputTokens += envelope.tokens()
		if envelope.RequestID != "" {
			raw.requestID = envelope.RequestID
		}
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
				"qwen: embedding %d is empty",
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
		Metadata: inference.Metadata{RequestID: raw.requestID},
	}, nil
}
