package dashscope

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Image generation runs on one of three surfaces, selected by the model
// declaration rather than by the model name:
//
//   - text2image (.../aigc/text2image/image-synthesis), an async task whose
//     terminal query carries output.results[].url;
//   - image2image (.../aigc/image2image/image-synthesis), the editing
//     surface: the same async task shape, with the edit function selecting
//     the operation and base_image_url plus mask_image_url naming its input;
//   - multimodal-generation (.../aigc/multimodal-generation/generation), the
//     synchronous call Wan 2.6 and later image models serve, which returns
//     its content array — text and image entries — inline. Without
//     interleaved output that surface edits: the platform requires the last
//     message to carry 1 to 4 input images. Interleaved output asks for the
//     stream — X-DashScope-Sse: enable paired with parameters.stream: true,
//     the only combination it accepts — and arrives as a sequence of events,
//     each carrying the content as far as it has come, which the transport
//     folds into one result: the terminal event carries the finished content
//     and the run's usage, and a refusal arrives as an event carrying the
//     error envelope, sometimes under an HTTP 200.
//
// Every surface delivers links, never bytes: task data is retained for 24
// hours, so the response carries the provider URL verbatim and downloading it
// is the caller's job.
const (
	imagePathText2Image  = "/api/v1/services/aigc/text2image/image-synthesis"
	imagePathImage2Image = "/api/v1/services/aigc/image2image/image-synthesis"
	imagePathMultimodal  = "/api/v1/services/aigc/multimodal-generation/generation"
)

type imageWire struct {
	model  string
	sync   bool
	edit   bool
	prompt string
	// negativePrompt, extend, and watermark are the knobs every surface
	// shares.
	negativePrompt string
	promptExtend   *bool
	watermark      *bool
	// images are the input images in request order. Where they land is the
	// surface's business: the editing surface reads base_image_url and
	// mask_image_url, the text2image surface reads ref_img, and the sync
	// surface carries them as content entries.
	images []string
	size   string
	count  int
	seed   *int64
	// format labels the returned links for the caller.
	format media.ImageFormat
	// style, refStrength, and refMode are the wanx-v1 reference-image
	// knobs.
	style       string
	refStrength *float64
	refMode     string
	// function, isSketch, and upscaleFactor are the editing surface's.
	function      string
	isSketch      *bool
	upscaleFactor *float64
	// enableInterleave switches the sync surface into interleaved
	// text-and-image output.
	enableInterleave bool
}

// imageItem is one entry of the sync surface's content array: exactly one of
// text and url is set, and the order of the entries is the order of the
// provider's interleaved output. The task surfaces fill it with URLs alone.
type imageItem struct {
	text string
	url  string
}

type imageRaw struct {
	items     []imageItem
	requestID string
	taskID    string
	format    media.ImageFormat
	usage     imageUsage
}

// imageUsage is the platform's media accounting, carried on both surfaces.
type imageUsage struct {
	imageCount int64
}

func compileImage(declared ModelSpec, name string) inference.GenerateCompiler[imageWire] {
	params := declared.Image
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[imageWire], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		if len(request.RequestMetadata) > 0 {
			ledger.Drop(
				inference.FieldGenerateRequestMetadata,
				"the DashScope image surfaces have no arbitrary request metadata channel",
			)
		}
		wire := imageWire{
			model:  wireModel(name, declared),
			sync:   params.API == imageAPISync,
			edit:   params.Edit,
			count:  1,
			format: params.Format,
		}
		if wire.format == "" {
			wire.format = media.ImageFormatPNG
		}
		if shape == inference.GenerateExecutionStream {
			ledger.Reject(
				inference.FieldGenerateExecutionStream,
				"image generation is unary on this provider",
			)
		}

		options, other := inference.ExtensionFor[ImageOptions](request.Extensions)

		prompt := turnParts(
			request,
			ledger,
			func(part message.Part, fields func(message.PartKind) inference.FieldID) {
				image, ok := part.(message.ImagePart)
				if !ok {
					ledger.Reject(
						fields(part.Kind()),
						fmt.Sprintf(
							"image generation accepts text and image parts, not %s",
							part.Kind(),
						),
					)
					return
				}
				switch image.Source.Kind() {
				case media.SourceURL, media.SourceInline:
				case media.SourceStream:
					ledger.Reject(
						fields(message.PartImage),
						"stream media sources must be materialized before generate",
					)
					return
				}
				if !params.acceptsInputImage(wire.sync) {
					ledger.Reject(
						fields(message.PartImage),
						"this text2image model has no input-image channel; declare reference_image, edit, or the sync surface",
					)
					return
				}
				value, err := imageSourceValue(image.Source)
				if err != nil {
					ledger.Reject(fields(message.PartImage), err.Error())
					return
				}
				wire.images = append(wire.images, value)
			},
			"image generation keeps user context only; assistant, system, and tool turns have no native channel",
		)
		wire.prompt = strings.Join(prompt, "\n")

		target := newRejectionTarget(request, shape)
		switch {
		case params.Edit && len(wire.images) == 0:
			target.reject(
				ledger,
				"an editing model requires an input image",
				inference.FieldGenerateIntentImage,
				inference.FieldGenerateInputImage,
			)
		case params.Edit && len(wire.images) > params.maxInputImages():
			target.reject(
				ledger,
				fmt.Sprintf(
					"the editing surface reads %d input images (base and mask), not %d",
					params.maxInputImages(), len(wire.images),
				),
				inference.FieldGenerateInputImage,
				inference.FieldGenerateIntentImage,
			)
		case wire.prompt == "" && !params.Edit:
			target.reject(
				ledger,
				"image generation requires a prompt",
				inference.FieldGenerateIntentImage,
				inference.FieldGenerateInputText,
			)
		}

		compileImageOptions(&wire, params, options, ledger, target)
		ledger.RejectExtensions("image generation", other)

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			rejectTextControls(
				text, ledger,
				"image models do not call tools",
				"the image API has no sampling controls",
				"image models have no thinking control",
			)
			switch {
			case wire.enableInterleave && text.MaxOutputTokens != nil:
				ledger.Reject(
					inference.FieldGenerateIntentTextMaxOutputTokens,
					"the image API has no output token budget",
				)
			case wire.enableInterleave:
			default:
				ledger.Reject(
					inference.FieldGenerateIntentText,
					"the image surfaces return images; only the sync surface with enable_interleave returns text",
				)
			}
			if wire.enableInterleave && text.Response != nil {
				ledger.Reject(
					inference.FieldGenerateIntentTextResponse,
					"the interleaved surface returns free-form text, not structured output",
				)
			}
		}
		if intent.Audio != nil {
			ledger.Reject(
				inference.FieldGenerateIntentAudio,
				"image models do not synthesize audio",
			)
		}
		if intent.Video != nil {
			ledger.Reject(
				inference.FieldGenerateIntentVideo,
				"image models do not generate video",
			)
		}
		if image := intent.Image; image != nil {
			if size := image.Size; size != nil {
				token, err := params.sizeToken(*size)
				if err != nil {
					ledger.Reject(
						inference.FieldGenerateIntentImageSize,
						err.Error(),
					)
				} else {
					wire.size = token
				}
			}
			if image.Count != nil {
				if params.CountMax > 0 && *image.Count > params.CountMax {
					ledger.Reject(
						inference.FieldGenerateIntentImageCount,
						fmt.Sprintf(
							"%s serves at most %d images per request, not %d",
							name, params.CountMax, *image.Count,
						),
					)
				} else {
					wire.count = *image.Count
				}
			}
			wire.seed = image.Seed
			if image.OutputFormat != "" && image.OutputFormat != wire.format {
				ledger.Reject(
					inference.FieldGenerateIntentImageOutputFormat,
					fmt.Sprintf(
						"%s delivers %s, not %s",
						name, wire.format, image.OutputFormat,
					),
				)
			}
			switch image.Delivery {
			case "", media.SourceURL:
			default:
				ledger.Reject(
					inference.FieldGenerateIntentImageDelivery,
					"the image surfaces deliver provider URLs; inline and stream delivery are unavailable",
				)
			}
			if image.Quality != "" {
				ledger.Drop(
					inference.FieldGenerateIntentImageQuality,
					"the image surfaces have no quality parameter",
				)
			}
		}
		rejectPromptLength(
			request, ledger,
			len([]rune(wire.prompt)), params.PromptLimit,
			fmt.Sprintf("%s caps the prompt at %d characters", name, params.PromptLimit),
		)

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[imageWire]{Report: report}, ledger.Err()
		}
		return inference.Compiled[imageWire]{Wire: wire, Report: report}, nil
	}
}

// acceptsInputImage reports whether the declared surface has a channel for an
// input image: the editing surface and the reference-image mode both do, and
// the sync multimodal surface carries them as content entries.
func (p ImageParams) acceptsInputImage(sync bool) bool {
	return p.Edit || p.ReferenceImage || sync
}

// maxInputImages is the number of input images the editing surface reads: a
// base image and its mask.
func (p ImageParams) maxInputImages() int {
	if p.Edit {
		return 2
	}
	return 0
}

// sizeToken validates a requested size against the declaration and renders
// it the way the wire wants it. An undeclared bound stays unvalidated — the
// endpoint's own validation decides — while a declared one is enforced here.
func (p ImageParams) sizeToken(size media.ImageSize) (string, error) {
	if len(p.Sizes) > 0 {
		accepted := false
		matched := false
		for _, declared := range p.Sizes {
			parsed, err := ParseSize(declared)
			if err != nil {
				continue
			}
			accepted = true
			if parsed == size {
				matched = true
				break
			}
		}
		if accepted && !matched {
			return "", fmt.Errorf(
				"the accepted sizes are %s, not %s",
				lists(p.Sizes), SizeToken(size),
			)
		}
	}
	if p.MinEdge > 0 && (size.Width < p.MinEdge || size.Height < p.MinEdge) {
		return "", fmt.Errorf(
			"each side must be at least %d, not %s", p.MinEdge, SizeToken(size),
		)
	}
	if p.MaxEdge > 0 && (size.Width > p.MaxEdge || size.Height > p.MaxEdge) {
		return "", fmt.Errorf(
			"each side must be at most %d, not %s", p.MaxEdge, SizeToken(size),
		)
	}
	pixels := int64(size.Width) * int64(size.Height)
	if p.MinPixels > 0 && pixels < p.MinPixels {
		return "", fmt.Errorf(
			"the size must cover at least %d pixels, not %s (%d)",
			p.MinPixels, SizeToken(size), pixels,
		)
	}
	if p.MaxPixels > 0 && pixels > p.MaxPixels {
		return "", fmt.Errorf(
			"the size must cover at most %d pixels, not %s (%d)",
			p.MaxPixels, SizeToken(size), pixels,
		)
	}
	return SizeToken(size), nil
}

// compileImageOptions lowers ImageOptions onto the wire. A setting the
// declaration does not enable is rejected: the model's surface has no such
// knob, so honoring it would silently change the request the caller believes
// they sent.
func compileImageOptions(
	wire *imageWire,
	params ImageParams,
	options ImageOptions,
	ledger *inference.Ledger,
	target rejectionTarget,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.Style != "" {
		if params.Style {
			wire.style = options.Style
		} else {
			ledger.Reject(
				field("style"),
				"this model does not declare the wanx-v1 style knob",
			)
		}
	}
	if options.Function != "" {
		if wire.edit {
			wire.function = options.Function
		} else {
			ledger.Reject(
				field("function"),
				"the function selector belongs to the editing surface, and this model is not an editing model",
			)
		}
	}
	if wire.edit && wire.function == "" {
		// The function selects the edit operation, so an editing model
		// cannot be lowered without one. The option the caller left unset
		// has no active field, so the rejection lands on the image the
		// operation would run on, or on the image intent when the request
		// declares one: the caller asked this model to produce an image
		// and never said how.
		target.reject(
			ledger,
			"an editing model requires the edit function; set image_options.function to the operation to run",
			inference.FieldGenerateInputImage,
			inference.FieldGenerateIntentImage,
		)
	}
	if options.RefStrength != nil {
		if params.ReferenceImage {
			wire.refStrength = options.RefStrength
		} else {
			ledger.Reject(
				field("ref_strength"),
				"this model has no reference-image channel",
			)
		}
	}
	if options.RefMode != "" {
		if params.ReferenceImage {
			wire.refMode = options.RefMode
		} else {
			ledger.Reject(
				field("ref_mode"),
				"this model has no reference-image channel",
			)
		}
	}
	if options.IsSketch != nil {
		wire.isSketch = options.IsSketch
	}
	if options.UpscaleFactor != nil {
		wire.upscaleFactor = options.UpscaleFactor
	}
	if options.EnableInterleave != nil {
		if wire.sync {
			wire.enableInterleave = *options.EnableInterleave
		} else {
			ledger.Reject(
				field("enable_interleave"),
				"interleaved output belongs to the sync surface, and this model rides the async task surface",
			)
		}
	}
	if options.NegativePrompt != "" {
		wire.negativePrompt = options.NegativePrompt
	}
	if options.PromptExtend != nil {
		wire.promptExtend = options.PromptExtend
	}
	if options.Watermark != nil {
		wire.watermark = options.Watermark
	}
}

// imageRequest renders the payload for the surface the wire selects.
func imageRequest(wire imageWire) map[string]any {
	parameters := map[string]any{"n": wire.count}
	if wire.size != "" {
		parameters["size"] = wire.size
	}
	if wire.seed != nil {
		parameters["seed"] = *wire.seed
	}
	if wire.promptExtend != nil {
		parameters["prompt_extend"] = *wire.promptExtend
	}
	if wire.watermark != nil {
		parameters["watermark"] = *wire.watermark
	}

	var input map[string]any
	switch {
	case wire.sync:
		content := make([]map[string]any, 0, len(wire.images)+1)
		content = append(content, map[string]any{"text": wire.prompt})
		for _, image := range wire.images {
			content = append(content, map[string]any{"image": image})
		}
		input = map[string]any{
			"messages": []map[string]any{{
				"role":    "user",
				"content": content,
			}},
		}
		if wire.negativePrompt != "" {
			parameters["negative_prompt"] = wire.negativePrompt
		}
		if wire.enableInterleave {
			parameters["enable_interleave"] = true
			// The interleaved surface pairs the sse header with this body
			// flag, and the platform refuses every other combination: the
			// header alone answers "currently stream=False is not supported",
			// the flag alone answers "x-dashscope-sse and stream parameter
			// mismatch". They are sent together or not at all.
			parameters["stream"] = true
		}
	case wire.edit:
		input = map[string]any{"function": wire.function}
		if wire.prompt != "" {
			input["prompt"] = wire.prompt
		}
		if wire.negativePrompt != "" {
			input["negative_prompt"] = wire.negativePrompt
		}
		if len(wire.images) > 0 {
			input["base_image_url"] = wire.images[0]
		}
		if len(wire.images) > 1 {
			input["mask_image_url"] = wire.images[1]
		}
		if wire.isSketch != nil {
			input["is_sketch"] = *wire.isSketch
		}
		if wire.upscaleFactor != nil {
			input["upscale_factor"] = *wire.upscaleFactor
		}
	default:
		input = map[string]any{"prompt": wire.prompt}
		if wire.negativePrompt != "" {
			input["negative_prompt"] = wire.negativePrompt
		}
		if len(wire.images) > 0 {
			input["ref_img"] = wire.images[0]
		}
		if wire.style != "" {
			parameters["style"] = wire.style
		}
		if wire.refStrength != nil {
			parameters["ref_strength"] = *wire.refStrength
		}
		if wire.refMode != "" {
			parameters["ref_mode"] = wire.refMode
		}
	}
	return map[string]any{
		"model":      wire.model,
		"input":      input,
		"parameters": parameters,
	}
}

// imageSyncResponse is the multimodal-generation envelope: the task
// pipeline's terminal state collapsed into one response, with the content
// array inline.
type imageSyncResponse struct {
	Output struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content []struct {
					Text  string `json:"text"`
					Image string `json:"image"`
				} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"output"`
	Usage struct {
		ImageCount int64 `json:"image_count"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

func transportImage(
	client *mediaClient,
	pollInterval time.Duration,
) inference.Transport[imageWire, imageRaw] {
	return func(ctx context.Context, wire imageWire) (imageRaw, error) {
		if wire.sync {
			return transportImageSync(ctx, client, wire)
		}
		path, operation := imagePathText2Image, "image generation"
		if wire.edit {
			path, operation = imagePathImage2Image, "image editing"
		}
		taskID, requestID, err := createTask(ctx, client, path, imageRequest(wire), operation)
		if err != nil {
			return imageRaw{}, err
		}
		query, err := awaitTask(ctx, client, pollInterval, taskID, operation)
		if err != nil {
			return imageRaw{}, err
		}
		urls, err := imageURLs(query)
		if err != nil {
			return imageRaw{}, err
		}
		items := make([]imageItem, 0, len(urls))
		for _, url := range urls {
			items = append(items, imageItem{url: url})
		}
		return imageRaw{
			items:     items,
			requestID: nonEmpty(query.RequestID, requestID),
			taskID:    taskID,
			format:    wire.format,
			usage:     imageUsage{imageCount: query.Usage.ImageCount},
		}, nil
	}
}

func transportImageSync(
	ctx context.Context,
	client *mediaClient,
	wire imageWire,
) (imageRaw, error) {
	events, err := client.postSSEEvents(
		ctx, imagePathMultimodal, imageRequest(wire), wire.enableInterleave,
	)
	if err != nil {
		return imageRaw{}, err
	}
	// The interleaved surface streams the run: each event carries the content
	// produced so far — text in fragments, images as their links land — and
	// the unary surfaces answer with the same envelope in one event, so both
	// fold into one result.
	var (
		items      []imageItem
		requestID  string
		imageCount int64
	)
	for _, event := range events {
		var response imageSyncResponse
		if err := json.Unmarshal(event, &response); err != nil {
			return imageRaw{}, fmt.Errorf(
				"dashscope: decode image generation event: %w", err)
		}
		if response.Code != "" || response.Message != "" {
			return imageRaw{}, errorBody{
				Code:      response.Code,
				Message:   response.Message,
				RequestID: response.RequestID,
			}.failure("image generation", 0)
		}
		if len(response.Output.Choices) == 0 {
			return imageRaw{}, fmt.Errorf(
				"dashscope: image generation returned no choices")
		}
		if response.RequestID != "" {
			requestID = response.RequestID
		}
		if response.Usage.ImageCount > 0 {
			imageCount = response.Usage.ImageCount
		}
		for _, entry := range response.Output.Choices[0].Message.Content {
			switch {
			case entry.Image != "":
				items = append(items, imageItem{url: entry.Image})
			case entry.Text != "":
				items = appendText(items, entry.Text)
			}
		}
	}
	return imageRaw{
		items:     items,
		requestID: requestID,
		format:    wire.format,
		usage:     imageUsage{imageCount: imageCount},
	}, nil
}

// appendText folds one streamed text fragment onto the trailing text item, so
// a run reads as one text part per stretch rather than one per fragment.
func appendText(items []imageItem, text string) []imageItem {
	if len(items) > 0 && items[len(items)-1].url == "" {
		items[len(items)-1].text += text
		return items
	}
	return append(items, imageItem{text: text})
}

func decodeImage(
	_ context.Context,
	raw imageRaw,
) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0, len(raw.items))
	images := int64(0)
	for _, item := range raw.items {
		switch {
		case item.url != "":
			source, err := media.NewImageURL(item.url, raw.format.MediaType())
			if err != nil {
				return inference.GenerateResponse{}, fmt.Errorf("dashscope: image url: %w", err)
			}
			parts = append(parts, message.ImagePart{Source: source})
			images++
		case item.text != "":
			parts = append(parts, message.TextPart{Text: item.text})
		}
	}
	if images == 0 {
		return inference.GenerateResponse{}, fmt.Errorf(
			"dashscope: image generation returned no images")
	}
	if raw.usage.imageCount > 0 {
		images = raw.usage.imageCount
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: inference.FinishCompleted,
		Usage:        inference.Usage{GeneratedImages: &images},
		Metadata: inference.Metadata{
			RequestID:  raw.requestID,
			ResponseID: raw.taskID,
		},
	}, nil
}

func openImage(
	cls *clients,
	spec Spec,
	declared ModelSpec,
	id model.ModelID,
) (inference.GenerateOperations, error) {
	unary, err := inference.BindGenerate(
		compileImage(declared, id.Name),
		transportImage(cls.media, spec.pollInterval()),
		decodeImage,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
