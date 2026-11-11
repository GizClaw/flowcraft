package dashscope

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

const (
	imagePathText2Image  = "/api/v1/services/aigc/text2image/image-synthesis"
	imagePathImage2Image = "/api/v1/services/aigc/image2image/image-synthesis"
)

type imageWire struct {
	model        string
	path         string
	prompt       string
	images       []string
	size         string
	count        int
	seed         *int64
	format       media.ImageFormat
	negative     string
	promptExtend *bool
	watermark    *bool
}

func imageOptions(extensions inference.Extensions) (*ImageOptions, error) {
	var found *ImageOptions
	for _, extension := range extensions {
		options, ok := extension.(*ImageOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: extension %s/%s is not understood", extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: duplicate image options"))
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

func compileImage(declared ModelSpec, name string) protocol.Compiler[inference.GenerateRequest, imageWire] {
	return func(_ context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[imageWire], error) {
		if request.Output.Kind() != inference.OutputImage {
			return protocol.Compiled[imageWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldOutput, fmt.Errorf("dashscope: image pipeline serves image output"))
		}
		if shape == protocol.ShapeStream {
			return protocol.Compiled[imageWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldStream, fmt.Errorf("dashscope: image generation is unary"))
		}
		options, err := imageOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[imageWire]{}, err
		}
		wire := imageWire{model: wireModel(name, declared), path: imagePathText2Image, count: 1, format: declared.Image.Format}
		if declared.Image.Edit {
			wire.path = imagePathImage2Image
		}
		if wire.format == "" {
			wire.format = media.ImageFormatPNG
		}
		var prompts []string
		collect := func(parts []message.Part) error {
			for _, part := range parts {
				switch value := part.(type) {
				case message.TextPart:
					prompts = append(prompts, value.Text)
				case message.ImagePart:
					if !declared.Image.Edit && !declared.Image.ReferenceImage {
						return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept image input"))
					}
					uri, err := imageSourceValue(value.Source)
					if err != nil {
						return err
					}
					wire.images = append(wire.images, uri)
				default:
					return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: image generation accepts text and image parts, not %s", part.Kind()))
				}
			}
			return nil
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				return protocol.Compiled[imageWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContext, fmt.Errorf("dashscope: image generation keeps user context only"))
			}
			if err := collect(turn.Content.Parts); err != nil {
				return protocol.Compiled[imageWire]{}, err
			}
		}
		if err := collect(request.Input.Content.Parts); err != nil {
			return protocol.Compiled[imageWire]{}, err
		}
		wire.prompt = strings.Join(prompts, "\n")
		if options != nil {
			if options.Size != nil {
				wire.size = fmt.Sprintf("%d*%d", options.Size.Width, options.Size.Height)
			}
			if options.Count != nil {
				wire.count = *options.Count
			}
			wire.seed = options.Seed
			if options.OutputFormat != "" {
				wire.format = options.OutputFormat
			}
			wire.negative = options.NegativePrompt
			wire.promptExtend = options.PromptExtend
			wire.watermark = options.Watermark
		}
		if declared.Image.CountMax > 0 && wire.count > declared.Image.CountMax {
			return protocol.Compiled[imageWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldExtensions, fmt.Errorf("dashscope: count exceeds model cap %d", declared.Image.CountMax))
		}
		return protocol.Compiled[imageWire]{Wire: wire, Fields: request.ActiveFields()}, nil
	}
}

func imageTaskRequest(wire imageWire) map[string]any {
	input := map[string]any{"prompt": wire.prompt}
	if len(wire.images) > 0 {
		if wire.path == imagePathImage2Image {
			input["base_image_url"] = wire.images[0]
		} else {
			input["ref_img"] = wire.images[0]
		}
	}
	parameters := map[string]any{"n": wire.count}
	if wire.size != "" {
		parameters["size"] = wire.size
	}
	if wire.seed != nil {
		parameters["seed"] = *wire.seed
	}
	if wire.negative != "" {
		parameters["negative_prompt"] = wire.negative
	}
	if wire.promptExtend != nil {
		parameters["prompt_extend"] = *wire.promptExtend
	}
	if wire.watermark != nil {
		parameters["watermark"] = *wire.watermark
	}
	return map[string]any{"model": wire.model, "input": input, "parameters": parameters}
}

func transportImage(client *mediaClient) protocol.Transport[inference.GenerateRequest, imageWire, imageRaw] {
	return func(ctx context.Context, _ inference.GenerateRequest, wire imageWire) (imageRaw, error) {
		taskID, requestID, err := createTask(ctx, client, wire.path, imageTaskRequest(wire), "image generation")
		if err != nil {
			return imageRaw{}, err
		}
		query, err := awaitTask(ctx, client, client.pollInterval(), taskID, "image generation")
		if err != nil {
			return imageRaw{}, err
		}
		urls, err := imageURLs(query)
		if err != nil {
			return imageRaw{}, err
		}
		return imageRaw{urls: urls, requestID: nonEmpty(query.RequestID, requestID), count: int64(len(urls))}, nil
	}
}

type imageRaw struct {
	urls      []string
	requestID string
	count     int64
}

func decodeImage(_ context.Context, request inference.GenerateRequest, raw imageRaw) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0, len(raw.urls))
	for _, url := range raw.urls {
		source, err := media.NewImageURL(url, "image/png")
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		parts = append(parts, message.ImagePart{Source: source})
	}
	return inference.GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: parts}}, FinishReason: inference.FinishStop,
		Usage:    usage.Usage{Work: map[usage.WorkKind]int64{usage.WorkImagesGenerated: raw.count}},
		Metadata: inference.Metadata{Model: request.Model.ID, RequestID: raw.requestID}}, nil
}

func (d *driver) compileImage(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[imageWire], error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[imageWire]{}, err
	}
	return compileImage(declared, request.Model.ID.Name)(ctx, request, shape)
}
