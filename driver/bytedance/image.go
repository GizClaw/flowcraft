package bytedance

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/driver/internal/ptr"

	"github.com/goccy/go-json"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

const generateImagesPath = "/images/generations"

type imageRequest struct {
	ark        arkmodel.GenerateImagesRequest
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
		compiled := &imageRequest{ark: arkmodel.GenerateImagesRequest{Model: endpoint}, count: 1}
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
				format := arkmodel.OutputFormat(options.OutputFormat)
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
		if options.SequentialMaxImages != nil {
			if options.Count != nil {
				return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
					fmt.Errorf("bytedance: sequential_max_images conflicts with count"))
			}
			maxImages = *options.SequentialMaxImages
		}
		if maxImages > 1 {
			compiled.ark.SequentialImageGenerationOptions =
				&arkmodel.SequentialImageGenerationOptions{MaxImages: &maxImages}
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
		compiled.ark.Tools = []*arkmodel.ContentGenerationTool{{Type: arkmodel.ToolTypeWebSearch}}
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

func generateOneImage(ctx context.Context, cls *clients, request *imageRequest) (imageRaw, error) {
	if request.background == "" {
		response, err := cls.ark.GenerateImages(ctx, request.ark, cls.arkRequestOptions...)
		if err != nil {
			return imageRaw{}, classifyError(err)
		}
		return imageRawFromResponse(response)
	}
	body, err := imageRequestMap(request.ark)
	if err != nil {
		return imageRaw{}, errdefs.Validation(fmt.Errorf("bytedance: image request: %w", err))
	}
	body["background"] = request.background
	response, err := cls.postImagesRaw(ctx, body)
	if err != nil {
		return imageRaw{}, err
	}
	return imageRawFromResponse(response)
}

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

func imageRawFromResponse(response arkmodel.ImagesResponse) (imageRaw, error) {
	if failure := response.Error; failure != nil {
		return imageRaw{}, classifyResponseError(failure.Code, failure.Message)
	}
	raw := imageRaw{requestID: response.Header().Get(arkmodel.ClientRequestHeader)}
	for _, image := range response.Data {
		raw.images = append(raw.images, rawImage{
			url: derefString(image.Url), b64: derefString(image.B64Json),
		})
	}
	if usage := response.Usage; usage != nil {
		raw.outputTokens = usage.OutputTokens
	}
	return raw, nil
}

func (c *clients) postImagesRaw(ctx context.Context, body map[string]any) (arkmodel.ImagesResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return arkmodel.ImagesResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+generateImagesPath, bytes.NewReader(payload))
	if err != nil {
		return arkmodel.ImagesResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return arkmodel.ImagesResponse{}, errdefs.NotAvailable(fmt.Errorf("bytedance: image request: %w", err))
	}
	defer func() { _ = response.Body.Close() }()
	requestID := response.Header.Get(arkmodel.ClientRequestHeader)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return arkmodel.ImagesResponse{}, imageRawError(response.StatusCode, requestID, response.Body)
	}
	var images arkmodel.ImagesResponse
	if err := json.NewDecoder(response.Body).Decode(&images); err != nil {
		return arkmodel.ImagesResponse{}, errdefs.NotAvailable(fmt.Errorf("bytedance: decode image response: %w", err))
	}
	images.SetHeader(response.Header)
	return images, nil
}

func imageRawError(status int, requestID string, body io.Reader) error {
	var envelope arkmodel.ErrorResponse
	if err := json.NewDecoder(body).Decode(&envelope); err != nil || envelope.Error == nil {
		return errdefs.WithRequestID(errdefs.ClassifyStatus(status,
			fmt.Errorf("%s: image request failed with status %d", providerID, status)), requestID)
	}
	failure := envelope.Error
	failure.HTTPStatusCode = status
	failure.RequestId = requestID
	return classifyError(failure)
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
		Usage: usage.Usage{
			InputTokens: raw.inputTokens, OutputTokens: raw.outputTokens,
			Work: map[usage.WorkKind]int64{usage.WorkImagesGenerated: generated},
		},
		Metadata: inference.Metadata{Model: request.Model.ID, RequestID: raw.requestID},
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
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
