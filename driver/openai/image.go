package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// Image generation runs on the images endpoint (gpt-image). The prompt is
// the request's text; the endpoint takes no reference images on this
// driver — image-to-image edits live on a separate API shape and are
// rejected truthfully. gpt-image always returns base64 payloads, so URL
// delivery has no native channel and is rejected.

type imageWire struct {
	model  string
	prompt string
	size   string // 1024x1024 | 1536x1024 | 1024x1536
	count  int
	format string // png | jpeg | webp
}

type imageRaw struct {
	images [][]byte
	// mediaType is the negotiated output format's media type; the provider
	// does not echo it, so the compiler-negotiated value is the truthful one.
	mediaType    string
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
}

// imageSizes are the only dimensions the endpoint accepts.
var imageSizes = map[string]struct{}{
	"1024x1024": {},
	"1536x1024": {},
	"1024x1536": {},
}

func compileImage(
	model string,
) inference.GenerateCompiler[imageWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[imageWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := imageWire{model: model, count: 1}
		if shape == inference.GenerateExecutionStream {
			ledger.reject(
				inference.FieldGenerateExecutionStream,
				"image generation is unary on this provider",
			)
		}

		var prompt []string
		collect := func(parts []message.Part, fields map[message.PartKind]inference.FieldID) {
			for _, part := range parts {
				switch value := part.(type) {
				case message.TextPart:
					prompt = append(prompt, value.Text)
				case message.ImagePart:
					ledger.reject(
						fields[message.PartImage],
						"image edits have no channel on this driver; text prompts only",
					)
				default:
					ledger.reject(
						fields[part.Kind()],
						fmt.Sprintf("image generation accepts text and image parts, not %s", part.Kind()),
					)
				}
			}
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				ledger.reject(
					inference.FieldGenerateContextRole,
					"image generation keeps user context only; assistant, system, and tool turns have no native channel",
				)
				continue
			}
			collect(turn.Content.Parts, contextPartFields)
		}
		collect(request.Input.Content.Parts, inputPartFields)
		wire.prompt = strings.Join(prompt, "\n")

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			rejectTextControls(text, ledger,
				"image models do not call tools",
				"the images API has no sampling controls",
				"image models have no reasoning control",
			)
			ledger.reject(
				inference.FieldGenerateIntentText,
				"image models do not produce text",
			)
		}
		if image := intent.Image; image != nil {
			if image.Size != nil {
				size := fmt.Sprintf("%dx%d", image.Size.Width, image.Size.Height)
				if _, ok := imageSizes[size]; ok {
					wire.size = size
				} else {
					ledger.reject(
						inference.FieldGenerateIntentImageSize,
						"the images API accepts 1024x1024, 1536x1024, or 1024x1536 only",
					)
				}
			}
			if image.AspectRatio != "" {
				ledger.reject(
					inference.FieldGenerateIntentImageAspectRatio,
					"the images API has no aspect-ratio parameter; give an explicit size",
				)
			}
			if image.Count != nil {
				wire.count = *image.Count
			}
			if image.Seed != nil {
				ledger.reject(
					inference.FieldGenerateIntentImageSeed,
					"the images API has no seed parameter",
				)
			}
			if image.OutputFormat != "" {
				switch image.OutputFormat {
				case media.ImageFormatPNG, media.ImageFormatJPEG, media.ImageFormatWebP:
					wire.format = string(image.OutputFormat)
				default:
					ledger.reject(
						inference.FieldGenerateIntentImageOutputFormat,
						fmt.Sprintf("image format %q is not supported", image.OutputFormat),
					)
				}
			}
			if image.Delivery == media.SourceURL {
				ledger.reject(
					inference.FieldGenerateIntentImageDelivery,
					"gpt-image returns inline payloads only; URL delivery has no channel",
				)
			}
		}
		for _, field := range request.Extensions.ActiveFields() {
			ledger.reject(field, "openai image generation supports no extensions")
		}
		if intent.Audio != nil {
			ledger.reject(
				inference.FieldGenerateIntentAudio,
				"image models do not synthesize audio",
			)
		}
		if intent.Video != nil {
			ledger.reject(
				inference.FieldGenerateIntentVideo,
				"image models do not generate video",
			)
		}
		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[imageWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[imageWire]{Wire: wire, Report: report}, nil
	}
}

func transportImage(
	client openai.Client,
) inference.Transport[imageWire, imageRaw] {
	return func(ctx context.Context, wire imageWire) (imageRaw, error) {
		params := openai.ImageGenerateParams{
			Model:  wire.model,
			Prompt: wire.prompt,
			N:      param.NewOpt(int64(wire.count)),
		}
		if wire.size != "" {
			params.Size = openai.ImageGenerateParamsSize(wire.size)
		}
		if wire.format != "" {
			params.OutputFormat = openai.ImageGenerateParamsOutputFormat(wire.format)
		}
		response, err := client.Images.Generate(ctx, params)
		if err != nil {
			return imageRaw{}, classifyError(err)
		}
		raw := imageRaw{
			mediaType:    media.ImageFormat(wire.format).MediaType(),
			inputTokens:  response.Usage.InputTokens,
			outputTokens: response.Usage.OutputTokens,
			totalTokens:  response.Usage.TotalTokens,
		}
		for index, image := range response.Data {
			data, err := base64.StdEncoding.DecodeString(image.B64JSON)
			if err != nil {
				return imageRaw{}, fmt.Errorf(
					"openai: decode image %d payload: %w",
					index,
					err,
				)
			}
			raw.images = append(raw.images, data)
		}
		return raw, nil
	}
}

func decodeImage(
	_ context.Context,
	raw imageRaw,
) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0, len(raw.images))
	for index, data := range raw.images {
		mediaType := raw.mediaType
		if mediaType == "" {
			// gpt-image always returns base64 payloads without a media type
			// echo; with no negotiated format, sniff the container so the
			// canonical part carries a truthful type.
			mediaType = sniffImageMediaType(data)
			if mediaType == "" {
				return inference.GenerateResponse{}, fmt.Errorf(
					"openai: image %d payload has unrecognized format",
					index,
				)
			}
		}
		source, err := media.NewImageBytes(data, mediaType)
		if err != nil {
			return inference.GenerateResponse{}, fmt.Errorf(
				"openai: image %d data: %w",
				index,
				err,
			)
		}
		parts = append(parts, message.ImagePart{Source: source})
	}
	generated := int64(len(raw.images))
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: inference.FinishCompleted,
		Usage: inference.Usage{
			InputTokens:     raw.inputTokens,
			OutputTokens:    raw.outputTokens,
			TotalTokens:     raw.totalTokens,
			GeneratedImages: &generated,
		},
	}, nil
}

func sniffImageMediaType(data []byte) string {
	switch {
	case len(data) >= 8 &&
		string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 &&
		data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 12 &&
		string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func openImage(
	cls *clients,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	unary, err := inference.BindGenerate(
		compileImage(id.Name),
		transportImage(cls.api),
		decodeImage,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
