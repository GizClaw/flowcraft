package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

// Image generation runs on the images endpoint (gpt-image). Text-only
// requests go to images/generations; requests that carry inline reference
// images go to images/edits, which uploads the bytes as multipart files
// (the official gpt-image-2 editing surface). URL-sourced reference images
// have no upload channel — callers must materialize them inline. gpt-image
// always returns base64 payloads, so URL delivery has no native channel and
// is rejected.

type imageWire struct {
	model         string
	prompt        string
	size          string // WxH; gpt-image-2-family models accept arbitrary sizes
	count         int
	format        string // png | jpeg | webp
	quality       string // auto | low | medium | high
	partialImages int    // 0-3 progress previews before the final image (stream only)
	// images are inline reference images for image-to-image edits; empty
	// means a text-only generation.
	images []wireImage
}

// wireImage is a concrete inline image payload: raw bytes plus the media
// type the compiler validated. Only inline sources reach the wire — URL and
// stream sources are rejected at compile time — so the source kind is
// constant and stays off the wire to satisfy the concrete-wire binding
// contract (core rejects wires containing interface values).
type wireImage struct {
	data      []byte
	mediaType string
}

// imageFile is a multipart file part that carries the image's media type;
// the openai-go multipart encoder reads ContentType() when present.
type imageFile struct {
	*bytes.Reader
	contentType string
}

func (f imageFile) ContentType() string { return f.contentType }

type imageRaw struct {
	images [][]byte
	// mediaType is the negotiated output format's media type; the provider
	// does not echo it, so the compiler-negotiated value is the truthful one.
	mediaType    string
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
	requestID    string
	responseID   string
}

// imageSize validates one canonical size against the gpt-image-2-family
// arbitrary resolution rules: both edges must be divisible by 16, the
// aspect ratio must stay between 1:3 and 3:1, and the resolution must not
// exceed the documented 3840x2160 maximum (long edge 3840, short edge
// 2160). The standard 1024x1024, 1536x1024, and 1024x1536 sizes satisfy
// the same rules. Resolutions above 2560x1440 are experimental but valid.
// On success the returned size is the canonical WxH wire string; the reason
// is non-empty when the size is rejected.
func imageSize(width, height int) (size, reason string) {
	if width <= 0 || height <= 0 {
		return "", "image dimensions must be positive"
	}
	if width%16 != 0 || height%16 != 0 {
		return "", "image width and height must both be divisible by 16"
	}
	max, min := width, height
	if max < min {
		max, min = min, max
	}
	if max > 3*min {
		return "", "image aspect ratio must be between 1:3 and 3:1"
	}
	if max > 3840 || min > 2160 {
		return "", "image resolution must not exceed 3840x2160"
	}
	return fmt.Sprintf("%dx%d", width, height), ""
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

		var prompt []string
		collect := func(parts []message.Part, fields map[message.PartKind]inference.FieldID) {
			for _, part := range parts {
				switch value := part.(type) {
				case message.TextPart:
					prompt = append(prompt, value.Text)
				case message.ImagePart:
					if value.Source.Kind() != media.SourceInline {
						ledger.reject(
							fields[message.PartImage],
							"images/edits uploads inline bytes; URL-sourced reference images have no channel",
						)
						continue
					}
					wire.images = append(wire.images, wireImage{
						data:      value.Source.Bytes(),
						mediaType: value.Source.BaseMediaType(),
					})
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
				if size, reason := imageSize(image.Size.Width, image.Size.Height); reason == "" {
					wire.size = size
				} else {
					ledger.reject(
						inference.FieldGenerateIntentImageSize,
						reason,
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
			if image.Quality != "" {
				switch image.Quality {
				case media.ImageQualityAuto,
					media.ImageQualityLow,
					media.ImageQualityMedium,
					media.ImageQualityHigh:
					wire.quality = string(image.Quality)
				default:
					ledger.reject(
						inference.FieldGenerateIntentImageQuality,
						fmt.Sprintf(
							"image quality %q is not supported",
							image.Quality,
						),
					)
				}
			}
		}
		options, other := operationExtensions[ImageOptions](request.Extensions)
		rejectOtherExtensions("image generation", other, ledger)
		if partial := options.PartialImages; partial != nil {
			field := inference.ExtensionField("partial_images").Qualify(options)
			switch {
			case shape != inference.GenerateExecutionStream:
				ledger.reject(
					field,
					"partial_images applies to the stream execution shape",
				)
			case *partial < 0 || *partial > 3:
				ledger.reject(
					field,
					fmt.Sprintf(
						"partial_images must be between 0 and 3, not %d",
						*partial,
					),
				)
			default:
				wire.partialImages = *partial
			}
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
		var response *openai.ImagesResponse
		var err error
		var requestID string
		if len(wire.images) == 0 {
			response, err = client.Images.Generate(
				ctx,
				imageGenerateParams(wire),
				captureRequestID(&requestID),
			)
		} else {
			response, err = client.Images.Edit(
				ctx,
				imageEditParams(wire),
				captureRequestID(&requestID),
			)
		}
		if err != nil {
			return imageRaw{}, classifyError(err)
		}
		raw := imageRaw{
			mediaType:    media.ImageFormat(wire.format).MediaType(),
			inputTokens:  response.Usage.InputTokens,
			outputTokens: response.Usage.OutputTokens,
			totalTokens:  response.Usage.TotalTokens,
			requestID:    requestID,
			responseID:   responseBodyID(response.JSON.ExtraFields),
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

// captureRequestID returns a per-call request option whose middleware
// records the provider's request-id response header. The middleware runs on
// every HTTP attempt, so the last attempt's header wins. OpenAI echoes the
// request identifier as x-request-id; apim-request-id covers Azure-backed
// deployments of the same protocol.
func captureRequestID(target *string) option.RequestOption {
	return option.WithMiddleware(func(
		req *http.Request,
		next option.MiddlewareNext,
	) (*http.Response, error) {
		response, err := next(req)
		if err != nil || response == nil {
			return response, err
		}
		if id := response.Header.Get("x-request-id"); id != "" {
			*target = id
		} else if id := response.Header.Get("apim-request-id"); id != "" {
			*target = id
		}
		return response, err
	})
}

// responseBodyID extracts a response-object id the SDK does not model from
// the decoded body's extra fields, when the provider echoes one.
func responseBodyID(fields map[string]respjson.Field) string {
	field, ok := fields["id"]
	if !ok || field.Raw() == "" {
		return ""
	}
	id, err := strconv.Unquote(field.Raw())
	if err != nil {
		return ""
	}
	return id
}

// imageGenerateParams renders the images/generations request body. Both the
// unary and streaming transports share it; partialImages stays zero off the
// stream shape, so unary requests never carry the streaming-only parameter.
func imageGenerateParams(wire imageWire) openai.ImageGenerateParams {
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
	if wire.quality != "" {
		params.Quality = openai.ImageGenerateParamsQuality(wire.quality)
	}
	if wire.partialImages > 0 {
		params.PartialImages = param.NewOpt(int64(wire.partialImages))
	}
	return params
}

// imageEditParams renders the images/edits multipart request body.
func imageEditParams(wire imageWire) openai.ImageEditParams {
	readers := make([]io.Reader, 0, len(wire.images))
	for _, image := range wire.images {
		readers = append(readers, imageFile{
			Reader:      bytes.NewReader(image.data),
			contentType: image.mediaType,
		})
	}
	params := openai.ImageEditParams{
		Model:  openai.ImageModel(wire.model),
		Prompt: wire.prompt,
		N:      param.NewOpt(int64(wire.count)),
		Image:  openai.ImageEditParamsImageUnion{OfFileArray: readers},
	}
	if wire.size != "" {
		params.Size = openai.ImageEditParamsSize(wire.size)
	}
	if wire.format != "" {
		params.OutputFormat = openai.ImageEditParamsOutputFormat(wire.format)
	}
	if wire.quality != "" {
		params.Quality = openai.ImageEditParamsQuality(wire.quality)
	}
	if wire.partialImages > 0 {
		params.PartialImages = param.NewOpt(int64(wire.partialImages))
	}
	return params
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
		Metadata: inference.Metadata{
			RequestID:  raw.requestID,
			ResponseID: raw.responseID,
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

// ---------------------------------------------------------------------------
// Streaming.
// ---------------------------------------------------------------------------

// imageStreamRaw is one event from the image streaming surface after part
// index assignment. interim marks a progress preview; the final image for
// an output arrives with interim false and carries the cumulative usage.
type imageStreamRaw struct {
	partIndex    int
	interim      bool
	b64          string
	outputFormat string
	usage        inference.Usage
	requestID    string
}

// imageStreamEvent is the driver-side view of one SDK image stream event.
// The SDK exposes generation and edit streams as two sealed unions with
// identical shapes; this view keeps both behind one concrete surface for
// the stream wrapper and decoder.
type imageStreamEvent struct {
	b64          string
	outputFormat string
	usage        inference.Usage
	partial      bool
	completed    bool
	rawType      string
}

// imageSDKStream is the small SDK surface the wrapper needs. The SDK stream
// types are concrete, so generation and edit streams each adapt it.
type imageSDKStream interface {
	Next() bool
	Current() imageStreamEvent
	Err() error
	Close() error
}

type imageGenSDKStream struct {
	stream *ssestream.Stream[openai.ImageGenStreamEventUnion]
}

func (s imageGenSDKStream) Next() bool { return s.stream.Next() }
func (s imageGenSDKStream) Err() error { return s.stream.Err() }
func (s imageGenSDKStream) Close() error {
	return s.stream.Close()
}

func (s imageGenSDKStream) Current() imageStreamEvent {
	event := s.stream.Current()
	return imageStreamEvent{
		b64:          event.B64JSON,
		outputFormat: event.OutputFormat,
		usage: inference.Usage{
			InputTokens:  event.Usage.InputTokens,
			OutputTokens: event.Usage.OutputTokens,
			TotalTokens:  event.Usage.TotalTokens,
		},
		partial:   event.Type == "image_generation.partial_image",
		completed: event.Type == "image_generation.completed",
		rawType:   event.Type,
	}
}

type imageEditSDKStream struct {
	stream *ssestream.Stream[openai.ImageEditStreamEventUnion]
}

func (s imageEditSDKStream) Next() bool { return s.stream.Next() }
func (s imageEditSDKStream) Err() error { return s.stream.Err() }
func (s imageEditSDKStream) Close() error {
	return s.stream.Close()
}

func (s imageEditSDKStream) Current() imageStreamEvent {
	event := s.stream.Current()
	return imageStreamEvent{
		b64:          event.B64JSON,
		outputFormat: event.OutputFormat,
		usage: inference.Usage{
			InputTokens:  event.Usage.InputTokens,
			OutputTokens: event.Usage.OutputTokens,
			TotalTokens:  event.Usage.TotalTokens,
		},
		partial:   event.Type == "image_edit.partial_image",
		completed: event.Type == "image_edit.completed",
		rawType:   event.Type,
	}
}

// imageStream adapts the SDK image SSE reader to ProviderStream[imageStreamRaw].
// It assigns canonical part indices: progress previews and the final image
// for one output share one index, so the terminal result keeps only the
// last image per output. The SDK events carry no per-image index, so the
// wrapper assumes the API streams outputs sequentially.
type imageStream struct {
	sdk       imageSDKStream
	nextPart  int
	requestID string
}

func (s *imageStream) Next(
	ctx context.Context,
) (imageStreamRaw, error) {
	if err := ctx.Err(); err != nil {
		return imageStreamRaw{}, errdefs.FromContext(err)
	}
	for {
		if !s.sdk.Next() {
			if err := s.sdk.Err(); err != nil {
				classified := classifyError(err)
				logInferenceStream(ctx, "generate", "", classified, "")
				return imageStreamRaw{}, classified
			}
			return imageStreamRaw{}, io.EOF
		}
		event := s.sdk.Current()
		switch {
		case event.partial:
			return imageStreamRaw{
				partIndex:    s.nextPart,
				interim:      true,
				b64:          event.b64,
				outputFormat: event.outputFormat,
			}, nil
		case event.completed:
			raw := imageStreamRaw{
				partIndex:    s.nextPart,
				b64:          event.b64,
				outputFormat: event.outputFormat,
				usage:        event.usage,
				requestID:    s.requestID,
			}
			s.nextPart++
			return raw, nil
		default:
			return imageStreamRaw{}, fmt.Errorf(
				"openai: unknown image stream event type %q",
				event.rawType,
			)
		}
	}
}

func (s *imageStream) Close() error {
	if s.sdk == nil {
		return nil
	}
	return classifyError(s.sdk.Close())
}

func transportImageStream(
	client openai.Client,
) inference.Transport[imageWire, inference.ProviderStream[imageStreamRaw]] {
	return func(
		ctx context.Context,
		wire imageWire,
	) (inference.ProviderStream[imageStreamRaw], error) {
		var sdk imageSDKStream
		var requestID string
		if len(wire.images) == 0 {
			sdk = imageGenSDKStream{
				stream: client.Images.GenerateStreaming(
					ctx,
					imageGenerateParams(wire),
					captureRequestID(&requestID),
				),
			}
		} else {
			sdk = imageEditSDKStream{
				stream: client.Images.EditStreaming(
					ctx,
					imageEditParams(wire),
					captureRequestID(&requestID),
				),
			}
		}
		if err := sdk.Err(); err != nil {
			classified := classifyError(err)
			logInferenceStream(ctx, "generate", wire.model, classified, "")
			return nil, classified
		}
		logInferenceStream(ctx, "generate", wire.model, nil, "")
		return &imageStream{sdk: sdk, requestID: requestID}, nil
	}
}

func decodeImageStream(
	_ context.Context,
	raw imageStreamRaw,
) (inference.GenerateStreamEvent, error) {
	image, err := streamImagePart(raw.b64, raw.outputFormat)
	if err != nil {
		return inference.GenerateStreamEvent{}, err
	}
	event := inference.GenerateStreamEvent{
		PartIndex: raw.partIndex,
		Delta: inference.ImagePartDelta{
			Part:    image,
			Interim: raw.interim,
		},
	}
	if !raw.interim {
		event.Usage = &raw.usage
		event.FinishReason = inference.FinishCompleted
		event.RequestID = raw.requestID
	}
	return event, nil
}

func streamImagePart(
	b64Data string,
	outputFormat string,
) (message.ImagePart, error) {
	data, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return message.ImagePart{}, fmt.Errorf(
			"openai: decode image stream payload: %w",
			err,
		)
	}
	mediaType := media.ImageFormat(outputFormat).MediaType()
	if mediaType == "" {
		// The stream events echo the negotiated output format; sniff the
		// container when it is absent or unknown.
		mediaType = sniffImageMediaType(data)
		if mediaType == "" {
			return message.ImagePart{}, fmt.Errorf(
				"openai: image stream payload has unrecognized format",
			)
		}
	}
	source, err := media.NewImageBytes(data, mediaType)
	if err != nil {
		return message.ImagePart{}, fmt.Errorf(
			"openai: image stream data: %w",
			err,
		)
	}
	return message.ImagePart{Source: source}, nil
}

func openImage(
	cls *clients,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileImage(id.Name),
		transportImage(cls.api),
		decodeImage,
		transportImageStream(cls.api),
		decodeImageStream,
	)
}
