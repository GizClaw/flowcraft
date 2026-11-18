package bytedance

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/outcome"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/driver/internal/httpx"
	"github.com/GizClaw/flowcraft/driver/internal/ptr"
)

const generateImagesPath = "/images/generations"

type imageRequest struct {
	ark        imageWire
	count      int
	background string
}

type imageRaw struct {
	images       []rawImage
	mediaType    string
	requestID    string
	inputTokens  int64
	outputTokens int64
}

type rawImage struct {
	url string
	b64 string
}

func imageOptions(extensions inference.Extensions) (*ImageOptions, error) {
	var found *ImageOptions
	for _, extension := range extensions {
		options, ok := extension.(*ImageOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: extension %s/%s is not understood",
					extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: duplicate image options"))
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

func compileImage(
	endpoint string,
	descriptor model.ModelDescriptor,
) protocol.Compiler[inference.GenerateRequest, *imageRequest] {
	return func(
		_ context.Context,
		request inference.GenerateRequest,
		shape protocol.Shape,
	) (protocol.Compiled[*imageRequest], error) {
		if request.Output.Kind() != inference.OutputImage {
			return protocol.Compiled[*imageRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldOutput,
				fmt.Errorf("bytedance: image pipeline serves image output only"))
		}
		if shape == protocol.ShapeStream {
			return protocol.Compiled[*imageRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldStream,
				fmt.Errorf("bytedance: image generation is unary"))
		}
		options, err := imageOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[*imageRequest]{}, err
		}
		delivery := "url"
		compiled := &imageRequest{ark: imageWire{Model: endpoint}, count: 1}
		var warnings []inference.Warning
		var prompt, references []string
		collect := func(parts []message.Part) error {
			for _, part := range parts {
				switch value := part.(type) {
				case message.TextPart:
					prompt = append(prompt, value.Text)
				case message.ImagePart:
					if !slices.Contains(descriptor.Capabilities.Inputs, message.PartImage) {
						return inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
							fmt.Errorf("bytedance: model does not accept image input"))
					}
					references = append(references, sourceURI(value.Source))
				default:
					return inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
						fmt.Errorf("bytedance: image generation accepts text and image parts, not %s", part.Kind()))
				}
			}
			return nil
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				return protocol.Compiled[*imageRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldContext,
					fmt.Errorf("bytedance: image generation keeps user context only"))
			}
			if err := collect(turn.Content.Parts); err != nil {
				return protocol.Compiled[*imageRequest]{}, err
			}
		}
		if err := collect(request.Input.Content.Parts); err != nil {
			return protocol.Compiled[*imageRequest]{}, err
		}
		compiled.ark.Prompt = strings.Join(prompt, "\n")
		if len(references) > 0 {
			compiled.ark.Image = references
		}
		if options != nil {
			if options.Size != nil {
				size := fmt.Sprintf("%dx%d", options.Size.Width, options.Size.Height)
				compiled.ark.Size = &size
			}
			if options.Count != nil {
				compiled.count = *options.Count
			}
			if options.Seed != nil {
				compiled.ark.Seed = ptr.Clone(options.Seed)
			}
			if options.OutputFormat != "" {
				format := string(options.OutputFormat)
				compiled.ark.OutputFormat = &format
			}
			switch options.Delivery {
			case media.SourceInline:
				delivery = "b64_json"
			case media.SourceURL:
				delivery = "url"
			}
			if options.Quality != "" {
				warnings = append(warnings, inference.Warning{
					Field:   inference.FieldExtensions,
					Code:    "quality_ignored",
					Message: "bytedance: seedream has no quality parameter",
				})
			}
			if compiled.ark.Seed != nil && compiled.count > 1 {
				return protocol.Compiled[*imageRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldExtensions,
					fmt.Errorf("bytedance: seed is supported for single-image requests only"))
			}
			if err := applyImageOptions(compiled, options); err != nil {
				return protocol.Compiled[*imageRequest]{}, err
			}
		}
		compiled.ark.ResponseFormat = &delivery
		return protocol.Compiled[*imageRequest]{
			Wire: compiled, Fields: request.ActiveFields(), Warnings: warnings,
		}, nil
	}
}

func applyImageOptions(compiled *imageRequest, options *ImageOptions) error {
	if options.GuidanceScale != nil {
		compiled.ark.GuidanceScale = ptr.Clone(options.GuidanceScale)
	}
	if options.Watermark != nil {
		compiled.ark.Watermark = ptr.Clone(options.Watermark)
	}
	if options.OptimizePrompt != nil {
		enabled := true
		compiled.ark.OptimizePrompt = &enabled
		optimize := &imageOptimizeOptions{}
		if options.OptimizePrompt.Mode != "" {
			optimize.Mode = options.OptimizePrompt.Mode
		}
		if options.OptimizePrompt.Thinking != "" {
			optimize.Thinking = options.OptimizePrompt.Thinking
		}
		compiled.ark.OptimizePromptOptions = optimize
	}
	if options.Sequential != nil && *options.Sequential {
		grouped := imageSequentialAuto
		compiled.ark.SequentialImageGeneration = &grouped
		maxImages := compiled.count
		if options.SequentialMaxImages != nil {
			if options.Count != nil {
				return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
					fmt.Errorf("bytedance: sequential_max_images conflicts with count"))
			}
			maxImages = *options.SequentialMaxImages
		}
		if maxImages > 1 {
			compiled.ark.SequentialImageGenerationOptions =
				&imageSequentialOptions{MaxImages: &maxImages}
		}
	}
	if options.SizeToken != "" {
		if options.Size != nil {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: size_token conflicts with size"))
		}
		size := arkSizeToken(options.SizeToken)
		compiled.ark.Size = &size
	}
	if options.WebSearch != nil && *options.WebSearch {
		compiled.ark.Tools = []*videoTool{{Type: "web_search"}}
	}
	if options.LayerDecomposition != nil && *options.LayerDecomposition {
		if len(imageReferences(compiled)) == 0 {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: layer_decomposition requires an input image"))
		}
		if compiled.ark.SequentialImageGeneration != nil {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: layer_decomposition conflicts with sequential generation"))
		}
		if options.Size != nil {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: layer_decomposition supports size_token only"))
		}
		compiled.ark.LayerDecomposition = ptr.Clone(options.LayerDecomposition)
	}
	if options.Background != "" {
		if len(imageReferences(compiled)) != 1 {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: background requires exactly one input image"))
		}
		if compiled.ark.SequentialImageGeneration != nil {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: background conflicts with sequential generation"))
		}
		compiled.background = options.Background
	}
	return nil
}

func imageReferences(compiled *imageRequest) []string {
	references, _ := compiled.ark.Image.([]string)
	return references
}

func arkSizeToken(token string) string {
	if token == "adaptive" {
		return "adaptive"
	}
	return strings.ToUpper(token)
}

func transportImage(cls *clients) protocol.Transport[inference.GenerateRequest, *imageRequest, imageRaw] {
	return func(ctx context.Context, _ inference.GenerateRequest, request *imageRequest) (imageRaw, error) {
		raw := imageRaw{mediaType: media.ImageFormat(derefOutputFormat(request.ark.OutputFormat)).MediaType()}
		count := request.count
		if request.ark.SequentialImageGeneration != nil {
			count = 1
		}
		for i := 0; i < count; i++ {
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
			raw.requestID = single.requestID
		}
		return raw, nil
	}
}

// generateOneImage posts one create call. The request id the platform stamps on
// the response header is what a caller quotes in a support thread, so it rides the
// raw answer.
func generateOneImage(ctx context.Context, cls *clients, request *imageRequest) (imageRaw, error) {
	response, err := cls.api.Do(ctx, httpx.Op{
		Method: http.MethodPost,
		Path:   generateImagesPath,
		Body:   request.ark,
		Query:  cls.query,
	})
	if err != nil {
		return imageRaw{}, err
	}
	defer func() { _ = response.Body.Close() }()
	requestID := response.Header.Get(imageRequestIDHeader)
	var images imagesResponse
	if err := httpx.DecodeJSON(generateImagesPath, response.Body, ArkResponseLimit, &images); err != nil {
		return imageRaw{}, err
	}
	return imageRawFromResponse(images, requestID)
}

// imageRawFromResponse reads the create answer: the failure envelope the platform
// may carry on an HTTP 200 first, then the images themselves.
func imageRawFromResponse(response imagesResponse, requestID string) (imageRaw, error) {
	if failure := response.Error; failure != nil {
		return imageRaw{}, errdefs.WithRequestID(
			classifyResponseError(failure.Code, failure.Message), requestID)
	}
	raw := imageRaw{requestID: requestID}
	for _, image := range response.Data {
		raw.images = append(raw.images, rawImage{url: image.URL, b64: image.B64JSON})
	}
	if usage := response.Usage; usage != nil {
		raw.outputTokens = usage.OutputTokens
	}
	return raw, nil
}

func decodeImage(
	_ context.Context,
	request inference.GenerateRequest,
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
			return inference.GenerateResponse{}, fmt.Errorf("bytedance: image %d carries neither url nor data", index)
		}
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		parts = append(parts, part)
	}
	generated := int64(len(raw.images))
	return inference.GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant, Content: message.Content{Parts: parts},
		},
		FinishReason: inference.FinishStop,
		Usage: outcome.Usage{
			InputTokens: raw.inputTokens, OutputTokens: raw.outputTokens,
			Work: map[outcome.WorkKind]int64{outcome.WorkImagesGenerated: generated},
		},
		Metadata: outcome.Metadata{Model: request.Model.ID, RequestID: raw.requestID},
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
		return message.ImagePart{}, fmt.Errorf("bytedance: decode image payload: %w", err)
	}
	mediaType := sniffImageMediaType(data)
	if mediaType == "" {
		return message.ImagePart{}, fmt.Errorf("bytedance: unrecognized image payload")
	}
	source, err := media.NewImageBytes(data, mediaType)
	if err != nil {
		return message.ImagePart{}, fmt.Errorf("bytedance: image data: %w", err)
	}
	return message.ImagePart{Source: source}, nil
}

func derefOutputFormat(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func sniffImageMediaType(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
