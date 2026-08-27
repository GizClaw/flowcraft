package minimax

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Image generation runs on the image_generation endpoint (image-01 family):
// one JSON request, one shot, no streaming. The prompt is the request's
// text; image parts become character subject references (image-to-image).
// The endpoint returns JPEGs only, so a requested output format other than
// JPEG is rejected rather than silently mislabeled. URL deliveries expire
// after 24 hours on the provider side.

type imageWire struct {
	model       string
	prompt      string
	references  []string
	aspectRatio string
	width       int
	height      int
	count       int
	seed        *int64
	delivery    string // url | base64
}

type imageRaw struct {
	urls      []string
	b64       []string
	mediaType string
	requestID string
}

// imageAspectRatios are the ratios image_generation accepts.
var imageAspectRatios = map[string]bool{
	"1:1": true, "16:9": true, "4:3": true, "3:2": true,
	"2:3": true, "3:4": true, "9:16": true, "21:9": true,
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
					wire.references = append(wire.references, imageSourceValue(value.Source))
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
				"the image API has no sampling controls",
				"image models have no thinking control",
			)
			ledger.reject(
				inference.FieldGenerateIntentText,
				"image models do not produce text",
			)
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
		if image := intent.Image; image != nil {
			if size := image.Size; size != nil {
				if size.Width < 512 || size.Width > 2048 ||
					size.Height < 512 || size.Height > 2048 ||
					size.Width%8 != 0 || size.Height%8 != 0 {
					ledger.reject(
						inference.FieldGenerateIntentImageSize,
						fmt.Sprintf(
							"minimax image dimensions are 512–2048 and divisible by 8, not %dx%d",
							size.Width, size.Height,
						),
					)
				} else {
					wire.width = size.Width
					wire.height = size.Height
				}
			}
			if ratio := string(image.AspectRatio); ratio != "" {
				if !imageAspectRatios[ratio] {
					ledger.reject(
						inference.FieldGenerateIntentImageAspectRatio,
						fmt.Sprintf("minimax image aspect ratios are 1:1/16:9/4:3/3:2/2:3/3:4/9:16/21:9, not %q", ratio),
					)
				} else {
					wire.aspectRatio = ratio
				}
			}
			if image.Count != nil {
				if *image.Count > 9 {
					ledger.reject(
						inference.FieldGenerateIntentImageCount,
						fmt.Sprintf("minimax image generation serves at most 9 images per request, not %d", *image.Count),
					)
				} else {
					wire.count = *image.Count
				}
			}
			wire.seed = image.Seed
			if image.OutputFormat != "" && image.OutputFormat != media.ImageFormatJPEG {
				ledger.reject(
					inference.FieldGenerateIntentImageOutputFormat,
					fmt.Sprintf("minimax image generation returns JPEG only, not %s", image.OutputFormat),
				)
			}
			switch image.Delivery {
			case "":
			case media.SourceURL:
				wire.delivery = "url"
			case media.SourceInline:
				wire.delivery = "base64"
			}
			if image.Quality != "" {
				ledger.drop(
					inference.FieldGenerateIntentImageQuality,
					"image-01 has no quality parameter",
				)
			}
		}
		rejectOtherExtensions("image generation", request.Extensions, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[imageWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[imageWire]{Wire: wire, Report: report}, nil
	}
}

// imageSourceValue renders a reference image the way subject_reference
// expects: absolute URLs pass through, inline bytes become base64.
func imageSourceValue(source media.ImageSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return base64.StdEncoding.EncodeToString(source.Bytes())
}

// imageRequest renders the image_generation payload.
func imageRequest(wire imageWire) map[string]any {
	request := map[string]any{
		"model":           wire.model,
		"prompt":          wire.prompt,
		"n":               wire.count,
		"response_format": wire.delivery,
	}
	if wire.aspectRatio != "" {
		request["aspect_ratio"] = wire.aspectRatio
	}
	if wire.width != 0 {
		request["width"] = wire.width
		request["height"] = wire.height
	}
	if wire.seed != nil {
		request["seed"] = *wire.seed
	}
	if len(wire.references) > 0 {
		references := make([]map[string]any, 0, len(wire.references))
		for _, reference := range wire.references {
			references = append(references, map[string]any{
				"type":       "character",
				"image_file": reference,
			})
		}
		request["subject_reference"] = references
	}
	return request
}

// imageResponse is the image_generation envelope.
type imageResponse struct {
	Data struct {
		URLs []string `json:"image_urls"`
		B64  []string `json:"image_base64"`
	} `json:"data"`
	// ID is the trace id for request tracking on the image_generation
	// endpoint.
	ID       string   `json:"id"`
	BaseResp baseResp `json:"base_resp"`
}

func transportImage(
	client *mediaClient,
) inference.Transport[imageWire, imageRaw] {
	return func(ctx context.Context, wire imageWire) (imageRaw, error) {
		var resp imageResponse
		if err := client.postJSON(ctx, "/v1/image_generation", imageRequest(wire), &resp); err != nil {
			return imageRaw{}, err
		}
		if err := resp.BaseResp.err("image generation"); err != nil {
			return imageRaw{}, err
		}
		return imageRaw{
			urls:      resp.Data.URLs,
			b64:       resp.Data.B64,
			mediaType: media.ImageFormatJPEG.MediaType(),
			requestID: resp.ID,
		}, nil
	}
}

func decodeImage(
	_ context.Context,
	raw imageRaw,
) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0, len(raw.urls)+len(raw.b64))
	for _, url := range raw.urls {
		source, err := media.NewImageURL(url, raw.mediaType)
		if err != nil {
			return inference.GenerateResponse{}, fmt.Errorf("minimax: image url: %w", err)
		}
		parts = append(parts, message.ImagePart{Source: source})
	}
	for _, encoded := range raw.b64 {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return inference.GenerateResponse{}, fmt.Errorf(
				"minimax: image payload is not valid base64: %w", err,
			)
		}
		source, err := media.NewImageBytes(data, raw.mediaType)
		if err != nil {
			return inference.GenerateResponse{}, fmt.Errorf("minimax: image payload: %w", err)
		}
		parts = append(parts, message.ImagePart{Source: source})
	}
	if len(parts) == 0 {
		return inference.GenerateResponse{}, fmt.Errorf(
			"minimax: image generation returned no images",
		)
	}
	generated := int64(len(parts))
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: inference.FinishCompleted,
		Usage: inference.Usage{
			GeneratedImages: &generated,
		},
		Metadata: inference.Metadata{RequestID: raw.requestID},
	}, nil
}

func openImage(
	cls *clients,
	_ catalogEntry,
	id inference.ModelID,
) (inference.GenerateOperations, error) {
	unary, err := inference.BindGenerate(
		compileImage(id.Name),
		transportImage(cls.media),
		decodeImage,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
