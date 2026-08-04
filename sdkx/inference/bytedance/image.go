package bytedance

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// Image generation runs on the Ark images endpoint (seedream). The prompt is
// the request's text; image parts become image-to-image references. The API
// returns one image per call, so a multi-count intent fans out into repeated
// calls in the transport; seeds are therefore only meaningful for
// single-image requests and the compiler rejects seed+count combinations
// instead of inventing per-image seed derivations.

type imageWire struct {
	model      string
	prompt     string
	references []string
	size       string // "<width>x<height>" or a named tier ("2K", "adaptive")
	count      int
	seed       *int64
	format     string // png | jpeg | webp
	delivery   string // url | b64_json
	// Extension settings (ImageOptions).
	guidanceScale  *float64
	watermark      *bool
	optimizePrompt *wireOptimizePrompt
	sequential     bool // grouped generation in one call
	sequentialMax  *int
	webSearch      bool
}

type wireOptimizePrompt struct {
	mode     string // standard | fast
	thinking string // auto | enabled | disabled
}

type imageRaw struct {
	images []rawImage
	// mediaType is the negotiated output format's media type; the provider
	// does not echo it, so the compiler-negotiated value is the truthful one.
	mediaType    string
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
}

type rawImage struct {
	url string
	b64 string
}

func compileImage(
	endpoint string,
) inference.GenerateCompiler[imageWire] {
	return func(
		_ context.Context,
		model inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[imageWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := imageWire{
			model:    endpoint,
			count:    1,
			delivery: "url",
		}
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
					wire.references = append(wire.references, sourceURI(value.Source))
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
				"image models have no thinking control",
			)
			ledger.reject(
				inference.FieldGenerateIntentText,
				"image models do not produce text",
			)
		}
		if image := intent.Image; image != nil {
			if image.Size != nil {
				wire.size = fmt.Sprintf("%dx%d", image.Size.Width, image.Size.Height)
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
				seed := *image.Seed
				wire.seed = &seed
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
			if image.Delivery != "" {
				switch image.Delivery {
				case media.SourceURL:
					wire.delivery = "url"
				case media.SourceInline:
					wire.delivery = "b64_json"
				}
			}
		}
		if wire.seed != nil && wire.count > 1 {
			ledger.reject(
				inference.FieldGenerateIntentImageSeed,
				"seed is supported for single-image requests only",
			)
		}
		options, other := operationExtensions[ImageOptions](request.Extensions)
		rejectOtherExtensions("image generation", other, ledger)
		compileImageOptions(&wire, options, intent.Image, ledger)
		if intent.Audio != nil {
			ledger.reject(
				inference.FieldGenerateIntentAudio,
				"image models do not synthesize audio",
			)
		}
		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[imageWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[imageWire]{Wire: wire, Report: report}, nil
	}
}

// compileImageOptions lowers ImageOptions onto the wire. Settings that
// collide with canonical intent fields are rejected instead of overriding:
// the canonical channel stays the single source of truth for what it covers.
func compileImageOptions(
	wire *imageWire,
	options ImageOptions,
	image *inference.ImageIntent,
	ledger *ledger,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.GuidanceScale != nil {
		wire.guidanceScale = options.GuidanceScale
	}
	if options.Watermark != nil {
		wire.watermark = options.Watermark
	}
	if options.OptimizePrompt != nil {
		wire.optimizePrompt = &wireOptimizePrompt{
			mode:     options.OptimizePrompt.Mode,
			thinking: options.OptimizePrompt.Thinking,
		}
	}
	if options.Sequential != nil && *options.Sequential {
		wire.sequential = true
		switch {
		case options.SequentialMaxImages != nil && image != nil && image.Count != nil:
			ledger.reject(
				field("sequential_max_images"),
				"the canonical count intent already bounds the group size",
			)
		case options.SequentialMaxImages != nil:
			wire.sequentialMax = options.SequentialMaxImages
		}
	}
	if options.SizeToken != "" {
		if image != nil && image.Size != nil {
			ledger.reject(
				field("size_token"),
				"the canonical size intent already selects dimensions",
			)
		} else {
			wire.size = arkSizeToken(options.SizeToken)
		}
	}
	if options.WebSearch != nil && *options.WebSearch {
		wire.webSearch = true
	}
}

// arkSizeToken normalizes the named size tier to the provider's casing.
func arkSizeToken(token string) string {
	if token == "adaptive" {
		return "adaptive"
	}
	return strings.ToUpper(token) // 1k | 2k | 4k
}

func transportImage(
	client *arkruntime.Client,
) inference.Transport[imageWire, imageRaw] {
	return func(ctx context.Context, wire imageWire) (imageRaw, error) {
		raw := imageRaw{mediaType: media.ImageFormat(wire.format).MediaType()}
		// Grouped generation returns its whole set from one call; only the
		// repeated-call path fans out per image.
		count := wire.count
		if wire.sequential {
			count = 1
		}
		for index := 0; index < count; index++ {
			if err := ctx.Err(); err != nil {
				return imageRaw{}, err
			}
			single, err := generateOneImage(ctx, client, wire)
			if err != nil {
				return imageRaw{}, err
			}
			raw.images = append(raw.images, single.images...)
			raw.inputTokens += single.inputTokens
			raw.outputTokens += single.outputTokens
			raw.totalTokens += single.totalTokens
		}
		return raw, nil
	}
}

func generateOneImage(
	ctx context.Context,
	client *arkruntime.Client,
	wire imageWire,
) (imageRaw, error) {
	request := arkmodel.GenerateImagesRequest{
		Model:          wire.model,
		Prompt:         wire.prompt,
		Seed:           wire.seed,
		ResponseFormat: &wire.delivery,
	}
	if len(wire.references) > 0 {
		request.Image = wire.references
	}
	if wire.size != "" {
		request.Size = &wire.size
	}
	if wire.format != "" {
		format := arkmodel.OutputFormat(wire.format)
		request.OutputFormat = &format
	}
	if wire.guidanceScale != nil {
		request.GuidanceScale = wire.guidanceScale
	}
	if wire.watermark != nil {
		request.Watermark = wire.watermark
	}
	if wire.optimizePrompt != nil {
		enabled := true
		request.OptimizePrompt = &enabled
		options := &arkmodel.OptimizePromptOptions{}
		if wire.optimizePrompt.mode != "" {
			mode := arkmodel.OptimizePromptMode(wire.optimizePrompt.mode)
			options.Mode = &mode
		}
		if wire.optimizePrompt.thinking != "" {
			thinking := arkmodel.OptimizePromptThinking(wire.optimizePrompt.thinking)
			options.Thinking = &thinking
		}
		request.OptimizePromptOptions = options
	}
	if wire.sequential {
		auto := arkmodel.SequentialImageGeneration(arkmodel.SequentialImageGenerationAuto)
		request.SequentialImageGeneration = &auto
		maxImages := wire.count
		if wire.sequentialMax != nil {
			maxImages = *wire.sequentialMax
		}
		if maxImages > 1 {
			request.SequentialImageGenerationOptions =
				&arkmodel.SequentialImageGenerationOptions{MaxImages: &maxImages}
		}
	}
	if wire.webSearch {
		request.Tools = []*arkmodel.ContentGenerationTool{{
			Type: arkmodel.ToolTypeWebSearch,
		}}
	}
	response, err := client.GenerateImages(ctx, request)
	if err != nil {
		return imageRaw{}, classifyError(err)
	}
	if failure := response.Error; failure != nil {
		return imageRaw{}, classifyResponseError(failure.Code, failure.Message)
	}
	raw := imageRaw{}
	for _, image := range response.Data {
		raw.images = append(raw.images, rawImage{
			url: derefString(image.Url),
			b64: derefString(image.B64Json),
		})
	}
	if usage := response.Usage; usage != nil {
		raw.outputTokens = usage.OutputTokens
		raw.totalTokens = usage.TotalTokens
	}
	return raw, nil
}

func decodeImage(
	_ context.Context,
	raw imageRaw,
) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0, len(raw.images))
	for index, image := range raw.images {
		var part message.ImagePart
		var err error
		switch {
		case image.url != "":
			part, err = imagePartFromURL(image.url, raw.mediaType)
		case image.b64 != "":
			part, err = imagePartFromB64(image.b64)
		default:
			return inference.GenerateResponse{}, fmt.Errorf(
				"bytedance: image %d carries neither url nor data",
				index,
			)
		}
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		parts = append(parts, part)
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

func imagePartFromURL(url, mediaType string) (message.ImagePart, error) {
	source, err := media.NewImageURL(url, mediaType)
	if err != nil {
		return message.ImagePart{}, fmt.Errorf("bytedance: image url: %w", err)
	}
	return message.ImagePart{Source: source}, nil
}

func imagePartFromB64(b64 string) (message.ImagePart, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return message.ImagePart{}, fmt.Errorf(
			"bytedance: decode image payload: %w",
			err,
		)
	}
	// The images API delivers base64 payloads without a media type; sniff the
	// container so the canonical part carries a truthful type.
	mediaType := sniffImageMediaType(data)
	if mediaType == "" {
		return message.ImagePart{}, fmt.Errorf(
			"bytedance: unrecognized image payload",
		)
	}
	source, err := media.NewImageBytes(data, mediaType)
	if err != nil {
		return message.ImagePart{}, fmt.Errorf("bytedance: image data: %w", err)
	}
	return message.ImagePart{Source: source}, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
	spec Spec,
	id inference.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	ark, err := cls.requireArk(profile)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	if err := cls.requireArkAPIKey(profile, "Seedream image generation"); err != nil {
		return inference.GenerateOperations{}, err
	}
	unary, err := inference.BindGenerate(
		compileImage(cls.endpoint(id.Name)),
		transportImage(ark),
		decodeImage,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
