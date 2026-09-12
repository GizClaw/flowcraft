package bytedance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/utils/ptr"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// generateImagesPath is the Ark images endpoint suffix; the SDK appends the
// same path to its base URL.
const generateImagesPath = "/images/generations"

// Image generation runs on the Ark images endpoint (seedream). The prompt is
// the request's text; image parts become image-to-image references. The API
// returns one image per call, so a multi-count intent fans out into repeated
// calls in the transport; seeds are therefore only meaningful for
// single-image requests and the compiler rejects seed+count combinations
// instead of inventing per-image seed derivations.

// imageRequest is one compiled Ark images call: the SDK request the transport
// posts, plus the two things that request cannot express — how many images the
// caller asked for, and the one official Seedream 5.0 pro field the SDK struct
// has no leaf for. Setting background forces the raw body path, which reuses
// the SDK request and adds it.
type imageRequest struct {
	ark arkmodel.GenerateImagesRequest
	// count is how many images the caller asked for. The endpoint returns one
	// image per call, so the transport repeats the call unless grouped
	// generation is on (then one call returns the whole set).
	count int
	// background has no SDK leaf; it rides the raw request path.
	background string
}

type imageRaw struct {
	images []rawImage
	// mediaType is the negotiated output format's media type; the provider
	// does not echo it, so the compiler-negotiated value is the truthful one.
	mediaType string
	// requestID is the provider-echoed request identifier from the response
	// header (X-Client-Request-Id). Empty when the endpoint does not return
	// one.
	requestID    string
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
) inference.GenerateCompiler[*imageRequest] {
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[*imageRequest], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		delivery := "url"
		compiled := &imageRequest{
			ark:   arkmodel.GenerateImagesRequest{Model: endpoint},
			count: 1,
		}
		if shape == inference.GenerateExecutionStream {
			ledger.Reject(
				inference.FieldGenerateExecutionStream,
				"image generation is unary on this provider",
			)
		}

		var prompt []string
		var references []string
		collect := func(parts []message.Part, fields func(message.PartKind) inference.FieldID) {
			for _, part := range parts {
				switch value := part.(type) {
				case message.TextPart:
					prompt = append(prompt, value.Text)
				case message.ImagePart:
					references = append(references, sourceURI(value.Source))
				default:
					ledger.Reject(
						fields(part.Kind()),
						fmt.Sprintf("image generation accepts text and image parts, not %s", part.Kind()),
					)
				}
			}
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				ledger.Reject(
					inference.FieldGenerateContextRole,
					"image generation keeps user context only; assistant, system, and tool turns have no native channel",
				)
				continue
			}
			collect(turn.Content.Parts, contextPartField)
		}
		collect(request.Input.Content.Parts, inputPartField)
		compiled.ark.Prompt = strings.Join(prompt, "\n")
		if len(references) > 0 {
			compiled.ark.Image = references
		}

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			rejectTextControls(text, ledger,
				"image models do not call tools",
				"the images API has no sampling controls",
				"image models have no thinking control",
			)
			ledger.Reject(
				inference.FieldGenerateIntentText,
				"image models do not produce text",
			)
		}
		if image := intent.Image; image != nil {
			if image.Size != nil {
				size := fmt.Sprintf("%dx%d", image.Size.Width, image.Size.Height)
				compiled.ark.Size = &size
			}
			if image.AspectRatio != "" {
				ledger.Reject(
					inference.FieldGenerateIntentImageAspectRatio,
					"the images API has no aspect-ratio parameter; give an explicit size",
				)
			}
			if image.Count != nil {
				compiled.count = *image.Count
			}
			if image.Seed != nil {
				compiled.ark.Seed = ptr.Clone(image.Seed)
			}
			if image.OutputFormat != "" {
				switch image.OutputFormat {
				case media.ImageFormatPNG, media.ImageFormatJPEG, media.ImageFormatWebP:
					format := arkmodel.OutputFormat(image.OutputFormat)
					compiled.ark.OutputFormat = &format
				default:
					ledger.Reject(
						inference.FieldGenerateIntentImageOutputFormat,
						fmt.Sprintf("image format %q is not supported", image.OutputFormat),
					)
				}
			}
			if image.Delivery != "" {
				switch image.Delivery {
				case media.SourceURL:
					delivery = "url"
				case media.SourceInline:
					delivery = "b64_json"
				}
			}
			if image.Quality != "" {
				ledger.Drop(
					inference.FieldGenerateIntentImageQuality,
					"seedream has no quality parameter; quality is set by model and resolution tier",
				)
			}
		}
		if compiled.ark.Seed != nil && compiled.count > 1 {
			ledger.Reject(
				inference.FieldGenerateIntentImageSeed,
				"seed is supported for single-image requests only",
			)
		}
		options, other := inference.ExtensionFor[ImageOptions](request.Extensions)
		ledger.RejectExtensions("image generation", other)
		compileImageOptions(compiled, options, intent.Image, ledger)
		// The endpoint delivers URLs unless the caller asked for inline
		// payloads; the request states the choice once, after every knob that
		// could change it has been read.
		compiled.ark.ResponseFormat = &delivery
		if intent.Audio != nil {
			ledger.Reject(
				inference.FieldGenerateIntentAudio,
				"image models do not synthesize audio",
			)
		}
		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[*imageRequest]{Report: report}, ledger.Err()
		}
		return inference.Compiled[*imageRequest]{Wire: compiled, Report: report}, nil
	}
}

// compileImageOptions lowers ImageOptions onto the request. Settings that
// collide with canonical intent fields are rejected instead of overriding:
// the canonical channel stays the single source of truth for what it covers.
func compileImageOptions(
	compiled *imageRequest,
	options ImageOptions,
	image *inference.ImageIntent,
	ledger *inference.Ledger,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.GuidanceScale != nil {
		compiled.ark.GuidanceScale = ptr.Clone(options.GuidanceScale)
	}
	if options.Watermark != nil {
		compiled.ark.Watermark = ptr.Clone(options.Watermark)
	}
	if options.OptimizePrompt != nil {
		enabled := true
		compiled.ark.OptimizePrompt = &enabled
		optimize := &arkmodel.OptimizePromptOptions{}
		if options.OptimizePrompt.Mode != "" {
			mode := arkmodel.OptimizePromptMode(options.OptimizePrompt.Mode)
			optimize.Mode = &mode
		}
		if options.OptimizePrompt.Thinking != "" {
			thinking := arkmodel.OptimizePromptThinking(options.OptimizePrompt.Thinking)
			optimize.Thinking = &thinking
		}
		compiled.ark.OptimizePromptOptions = optimize
	}
	if options.Sequential != nil && *options.Sequential {
		grouped := arkmodel.SequentialImageGeneration(arkmodel.SequentialImageGenerationAuto)
		compiled.ark.SequentialImageGeneration = &grouped
		maxImages := compiled.count
		switch {
		case options.SequentialMaxImages != nil && image != nil && image.Count != nil:
			ledger.Reject(
				field("sequential_max_images"),
				"the canonical count intent already bounds the group size",
			)
		case options.SequentialMaxImages != nil:
			maxImages = *options.SequentialMaxImages
		}
		if maxImages > 1 {
			compiled.ark.SequentialImageGenerationOptions =
				&arkmodel.SequentialImageGenerationOptions{MaxImages: &maxImages}
		}
	}
	if options.SizeToken != "" {
		if image != nil && image.Size != nil {
			ledger.Reject(
				field("size_token"),
				"the canonical size intent already selects dimensions",
			)
		} else {
			size := arkSizeToken(options.SizeToken)
			compiled.ark.Size = &size
		}
	}
	if options.WebSearch != nil && *options.WebSearch {
		compiled.ark.Tools = []*arkmodel.ContentGenerationTool{{
			Type: arkmodel.ToolTypeWebSearch,
		}}
	}
	if options.LayerDecomposition != nil && *options.LayerDecomposition {
		switch {
		case len(imageReferences(compiled)) == 0:
			ledger.Reject(
				field("layer_decomposition"),
				"layer decomposition requires an input image",
			)
		case compiled.ark.SequentialImageGeneration != nil:
			ledger.Reject(
				field("layer_decomposition"),
				"layer decomposition is not supported with sequential generation",
			)
		case image != nil && image.Size != nil:
			ledger.Reject(
				field("layer_decomposition"),
				"layer decomposition supports resolution levels only; use size_token instead of explicit dimensions",
			)
		default:
			compiled.ark.LayerDecomposition = ptr.Clone(options.LayerDecomposition)
		}
	}
	if options.Background != "" {
		switch {
		case len(imageReferences(compiled)) != 1:
			ledger.Reject(
				field("background"),
				"background requires exactly one input image with an alpha channel",
			)
		case compiled.ark.SequentialImageGeneration != nil:
			ledger.Reject(
				field("background"),
				"background is not supported with sequential generation",
			)
		default:
			compiled.background = options.Background
		}
	}
}

// imageReferences returns the request's inline reference images. The SDK field
// is an interface because it also accepts a single image or a token list; the
// compiler only ever fills it with the URL list.
func imageReferences(compiled *imageRequest) []string {
	references, _ := compiled.ark.Image.([]string)
	return references
}

// arkSizeToken normalizes the named size tier to the provider's casing.
func arkSizeToken(token string) string {
	if token == "adaptive" {
		return "adaptive"
	}
	return strings.ToUpper(token) // 1k | 1.5k | 2k | 3k | 4k
}

func transportImage(cls *clients) inference.Transport[*imageRequest, imageRaw] {
	return func(ctx context.Context, request *imageRequest) (imageRaw, error) {
		raw := imageRaw{
			mediaType: media.ImageFormat(string(derefOutputFormat(request.ark.OutputFormat))).MediaType(),
		}
		// Grouped generation returns its whole set from one call; only the
		// repeated-call path fans out per image.
		count := request.count
		if request.ark.SequentialImageGeneration != nil {
			count = 1
		}
		for index := 0; index < count; index++ {
			if err := ctx.Err(); err != nil {
				return imageRaw{}, err
			}
			single, err := generateOneImage(ctx, cls, request)
			if err != nil {
				return imageRaw{}, err
			}
			raw.images = append(raw.images, single.images...)
			raw.inputTokens += single.inputTokens
			raw.outputTokens += single.outputTokens
			raw.totalTokens += single.totalTokens
			raw.requestID = single.requestID
		}
		return raw, nil
	}
}

func generateOneImage(
	ctx context.Context,
	cls *clients,
	request *imageRequest,
) (imageRaw, error) {
	if request.background == "" {
		response, err := cls.ark.GenerateImages(
			ctx,
			request.ark,
			cls.arkRequestOptions...,
		)
		if err != nil {
			return imageRaw{}, classifyError(err)
		}
		return imageRawFromResponse(response)
	}
	body, err := imageRequestMap(request.ark)
	if err != nil {
		return imageRaw{}, errdefs.Validation(fmt.Errorf("bytedance: image request: %w", err))
	}
	// layer_decomposition rides the SDK request above; the pinned SDK struct
	// has no background leaf, so the raw body carries it.
	body["background"] = request.background
	response, err := cls.postImagesRaw(ctx, body)
	if err != nil {
		return imageRaw{}, err
	}
	return imageRawFromResponse(response)
}

// imageRequestMap serializes the SDK request exactly as GenerateImages would,
// so the raw path carries the same request body plus the field the SDK struct
// cannot encode.
func imageRequestMap(request arkmodel.GenerateImagesRequest) (map[string]any, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// imageRawFromResponse folds an ImagesResponse into the transport raw shape.
func imageRawFromResponse(response arkmodel.ImagesResponse) (imageRaw, error) {
	if failure := response.Error; failure != nil {
		return imageRaw{}, classifyResponseError(failure.Code, failure.Message)
	}
	raw := imageRaw{requestID: response.Header().Get(arkmodel.ClientRequestHeader)}
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

// postImagesRaw issues a raw POST to the Ark images endpoint. The SDK request
// struct cannot encode background, so requests carrying it take this path,
// reusing the profile's API key, base URL, and retry/timeout HTTP client so
// behavior matches the SDK path.
func (c *clients) postImagesRaw(
	ctx context.Context,
	body map[string]any,
) (arkmodel.ImagesResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return arkmodel.ImagesResponse{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+generateImagesPath,
		bytes.NewReader(payload),
	)
	if err != nil {
		return arkmodel.ImagesResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return arkmodel.ImagesResponse{}, errdefs.NotAvailable(
			fmt.Errorf("bytedance: image request: %w", err),
		)
	}
	defer func() { _ = response.Body.Close() }()
	requestID := response.Header.Get(arkmodel.ClientRequestHeader)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return arkmodel.ImagesResponse{}, imageRawError(
			response.StatusCode, requestID, response.Body,
		)
	}
	var images arkmodel.ImagesResponse
	if err := json.NewDecoder(response.Body).Decode(&images); err != nil {
		return arkmodel.ImagesResponse{}, errdefs.NotAvailable(
			fmt.Errorf("bytedance: decode image response: %w", err),
		)
	}
	// The SDK path attaches response headers onto the decoded struct; do the
	// same here so the caller can read the echoed request id uniformly.
	images.SetHeader(response.Header)
	return images, nil
}

// imageRawError classifies a failed raw image request the same way the SDK
// does: the JSON error envelope wins when present, status-code taxonomy
// otherwise.
func imageRawError(status int, requestID string, body io.Reader) error {
	var envelope arkmodel.ErrorResponse
	if err := json.NewDecoder(body).Decode(&envelope); err != nil || envelope.Error == nil {
		return errdefs.WithRequestID(
			errdefs.ClassifyStatus(
				status,
				fmt.Errorf("%s: image request failed with status %d", providerID, status),
			),
			requestID,
		)
	}
	failure := envelope.Error
	failure.HTTPStatusCode = status
	failure.RequestId = requestID
	return classifyError(failure)
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
		Metadata: inference.Metadata{RequestID: raw.requestID},
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

// derefOutputFormat reads the negotiated output format. An unset format means
// the provider default applies, and the decoder derives the payload's media
// type from the payload itself.
func derefOutputFormat(value *arkmodel.OutputFormat) arkmodel.OutputFormat {
	if value == nil {
		return ""
	}
	return *value
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
	id model.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	if _, err := cls.requireArk(profile); err != nil {
		return inference.GenerateOperations{}, err
	}
	unary, err := inference.BindGenerate(
		compileImage(cls.endpoint(id.Name)),
		transportImage(cls),
		decodeImage,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
