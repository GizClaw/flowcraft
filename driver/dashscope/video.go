package dashscope

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Video generation is an async task on either of two endpoints, selected by
// the declared input shape rather than by the model name:
//
//   - video-synthesis (.../aigc/video-generation/video-synthesis) for
//     text-to-video and image-to-video, where the input image rides
//     input.img_url;
//   - image2video (.../aigc/image2video/video-synthesis) for first/last-frame
//     generation, where the frames ride input.first_frame_url and
//     input.last_frame_url.
//
// A declaration that names the media array (Wan 3.0's all-in-one models)
// rides video-synthesis either way and carries its assets as input.media
// entries instead of those flat keys — frames included, since that surface
// takes first_frame and last_frame as entry types.
//
// Both answer with output.video_url on the terminal query, and both retain
// their result for 24 hours; the response carries the link and downloading it
// is the caller's job.
const (
	videoPathSynthesis = "/api/v1/services/aigc/video-generation/video-synthesis"
	videoPathImage2    = "/api/v1/services/aigc/image2video/video-synthesis"

	// videoMediaType labels the MP4 (H.264) every Wan video surface
	// returns.
	videoMediaType = "video/mp4"

	// defaultVideoRatio is the platform's own default ratio for the
	// text-to-video surfaces. A size-param model needs a ratio to resolve
	// its size table, and an unspecified one has to mean the same thing the
	// endpoint would have chosen.
	defaultVideoRatio = "16:9"

	// smartDurationSeconds is the parameters.duration the media surface
	// reads as "let the model pick the length from the prompt and media".
	smartDurationSeconds = -1
)

type videoWire struct {
	model string
	// keyframes selects the image2video endpoint; otherwise the wire rides
	// video-synthesis.
	keyframes      bool
	prompt         string
	negativePrompt string
	// imgURL is the i2v input image; firstFrame and lastFrame are the kf2v
	// pair.
	imgURL     string
	firstFrame string
	lastFrame  string
	// audioURL is the input audio a model declaring audio accepts.
	audioURL string
	// template names a video-effect template, which replaces the prompt as
	// the driver of the generation.
	template string
	// resolution carries either the tier token (480P/720P/1080P) or the
	// concrete 宽*高 a size-param model wants.
	resolution string
	// sizeConcrete marks resolution as a 宽*高 the size parameter carries
	// rather than a tier the resolution parameter carries.
	sizeConcrete bool
	ratio        string
	duration     *int
	seed         *int64
	watermark    *bool
	promptExtend *bool
	shotType     string
	audioSwitch  *bool
	// media carries the Wan 3.0 media-array entries in request order: the
	// platform numbers each type by its position in the array and the
	// prompt refers to those numbers.
	media []mediaEntry
	// link is the public web page the caller passes through
	// video_options.link, rendered as the last array entry.
	link string
	// smartDuration asks for duration -1, the model-chosen length.
	smartDuration bool
}

// hasEntry reports whether a media entry of that type rides the request.
func (w videoWire) hasEntry(entryType string) bool {
	for _, entry := range w.media {
		if entry.Type == entryType {
			return true
		}
	}
	return false
}

type videoRaw struct {
	requestID  string
	taskID     string
	url        string
	videoCount int64
}

func compileVideo(declared ModelSpec, name string) inference.GenerateCompiler[videoWire] {
	params := declared.Video
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[videoWire], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		if len(request.RequestMetadata) > 0 {
			ledger.Drop(
				inference.FieldGenerateRequestMetadata,
				"the DashScope video surfaces have no arbitrary request metadata channel",
			)
		}
		wire := videoWire{
			model: wireModel(name, declared),
			// A media declaration always rides the synthesis endpoint: the
			// array carries frames on the same surface as everything else,
			// unlike the 2.x kf2v endpoint.
			keyframes: params.Media == nil && params.Input == videoInputKeyframes,
		}
		assets := newMediaInput(params.Media)
		if shape == inference.GenerateExecutionStream {
			ledger.Reject(
				inference.FieldGenerateExecutionStream,
				"video generation is unary on this provider",
			)
		}

		options, other := inference.ExtensionFor[VideoOptions](request.Extensions)

		prompt := turnParts(
			request,
			ledger,
			func(part message.Part, fields func(message.PartKind) inference.FieldID) {
				switch value := part.(type) {
				case message.ImagePart:
					if assets.enabled() {
						assets.acceptImage(value.Source, fields(message.PartImage), ledger)
						return
					}
					if params.Input != videoInputImage && params.Input != videoInputKeyframes {
						ledger.Reject(
							fields(message.PartImage),
							fmt.Sprintf(
								"this model declares input %q, which takes no input image; declare i2v or kf2v for the image surfaces",
								nonEmpty(params.Input, videoInputText),
							),
						)
						return
					}
					switch value.Source.Kind() {
					case media.SourceURL, media.SourceInline:
					case media.SourceStream:
						ledger.Reject(
							fields(message.PartImage),
							"stream media sources must be materialized before generate",
						)
						return
					}
					source, err := imageSourceValue(value.Source)
					if err != nil {
						ledger.Reject(fields(message.PartImage), err.Error())
						return
					}
					switch {
					case wire.keyframes && wire.firstFrame == "":
						wire.firstFrame = source
					case wire.keyframes:
						wire.lastFrame = source
					case wire.imgURL == "":
						wire.imgURL = source
					default:
						ledger.Reject(
							fields(message.PartImage),
							fmt.Sprintf(
								"the i2v surface takes one input image (img_url), not %d; first/last-frame pairs need the kf2v surface",
								len(wire.frames())+1,
							),
						)
					}
				case message.AudioPart:
					if assets.enabled() {
						assets.acceptAudio(value.Source, fields(message.PartAudio), ledger)
						return
					}
					if !params.Audio {
						ledger.Reject(
							fields(message.PartAudio),
							"this model does not declare audio input",
						)
						return
					}
					switch value.Source.Kind() {
					case media.SourceURL, media.SourceInline:
					case media.SourceStream:
						ledger.Reject(
							fields(message.PartAudio),
							"stream media sources must be materialized before generate",
						)
						return
					}
					source, err := audioSourceValue(value.Source)
					if err != nil {
						ledger.Reject(fields(message.PartAudio), err.Error())
						return
					}
					wire.audioURL = source
				case message.VideoPart:
					if assets.enabled() {
						assets.acceptVideo(value.Source, fields(message.PartVideo), ledger)
						return
					}
					ledger.Reject(
						fields(message.PartVideo),
						"video generation accepts text, image, and audio parts, not video",
					)
				case message.FilePart:
					if assets.enabled() {
						assets.acceptFile(value, fields(message.PartFile), ledger)
						return
					}
					ledger.Reject(
						fields(message.PartFile),
						"video generation accepts text, image, and audio parts, not file",
					)
				default:
					ledger.Reject(
						fields(part.Kind()),
						fmt.Sprintf(
							"video generation accepts text, image, and audio parts, not %s",
							part.Kind(),
						),
					)
				}
			},
			"video generation keeps user context only; assistant, system, and tool turns have no native channel",
		)
		wire.prompt = strings.Join(prompt, "\n")
		wire.media = assets.entries
		rejectPromptLength(
			request,
			ledger,
			len([]rune(wire.prompt)),
			params.PromptLimit,
			fmt.Sprintf("%s caps the prompt at %d characters", name, params.PromptLimit),
		)

		target := newRejectionTarget(request, shape)
		switch {
		case params.Media != nil && wire.prompt == "" && len(wire.media) == 0:
			target.reject(
				ledger,
				"the media surface requires a prompt or at least one media entry",
				inference.FieldGenerateInputText,
				inference.FieldGenerateContextText,
				inference.FieldGenerateIntentVideo,
			)
		case params.Media != nil:
			// Either one carries the request on its own: the platform
			// takes a prompt, media, or both, and the array is what makes
			// the frames and reference modes reachable.
		case params.Input == videoInputImage && len(wire.frames()) == 0:
			target.reject(
				ledger,
				"the i2v surface requires an input image (img_url)",
				inference.FieldGenerateIntentVideo,
				inference.FieldGenerateInputImage,
			)
		case params.Input == videoInputKeyframes && len(wire.frames()) == 0:
			target.reject(
				ledger,
				"the kf2v surface requires a first frame",
				inference.FieldGenerateIntentVideo,
				inference.FieldGenerateInputImage,
			)
		case params.Input == videoInputImage || params.Input == videoInputKeyframes:
		case wire.prompt == "":
			target.reject(
				ledger,
				"video generation requires a prompt",
				inference.FieldGenerateInputText,
				inference.FieldGenerateContextText,
				inference.FieldGenerateIntentVideo,
			)
		}

		compileVideoOptions(&wire, params, options, ledger)
		ledger.RejectExtensions("video generation", other)

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			rejectTextControls(
				text, ledger,
				"video models do not call tools",
				"the video API has no sampling controls",
				"video models have no thinking control",
			)
			ledger.Reject(
				inference.FieldGenerateIntentText,
				"video models do not produce text",
			)
		}
		if intent.Audio != nil {
			ledger.Reject(
				inference.FieldGenerateIntentAudio,
				"video models do not synthesize speech; declare video.audio_switch for a soundtrack",
			)
		}
		if intent.Image != nil {
			ledger.Reject(
				inference.FieldGenerateIntentImage,
				"video models do not generate images",
			)
		}
		if video := intent.Video; video != nil {
			compileVideoIntent(&wire, params, name, video, ledger, target)
		}

		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[videoWire]{Report: report}, ledger.Err()
		}
		return inference.Compiled[videoWire]{Wire: wire, Report: report}, nil
	}
}

// frames lists the input frames in request order.
func (w videoWire) frames() []string {
	frames := make([]string, 0, 2)
	for _, frame := range []string{w.imgURL, w.firstFrame, w.lastFrame} {
		if frame != "" {
			frames = append(frames, frame)
		}
	}
	return frames
}

// compileVideoIntent lowers the canonical video intent onto the wire,
// enforcing every fact the declaration names and leaving an undeclared fact
// to the endpoint.
func compileVideoIntent(
	wire *videoWire,
	params VideoParams,
	name string,
	video *inference.VideoIntent,
	ledger *inference.Ledger,
	target rejectionTarget,
) {
	ratio := string(video.AspectRatio)
	if ratio != "" && !params.declaresRatio(ratio) {
		ledger.Reject(
			inference.FieldGenerateIntentVideoAspectRatio,
			fmt.Sprintf(
				"%s accepts the ratios %s, not %s",
				name, params.ratioHint(), ratio,
			),
		)
	}
	tier := ""
	if video.Resolution != "" {
		tier = NormalizeResolution(video.Resolution)
		if !params.declaresResolution(tier) {
			ledger.Reject(
				inference.FieldGenerateIntentVideoResolution,
				fmt.Sprintf(
					"%s accepts the resolutions %s, not %s",
					name, lists(params.Resolutions), video.Resolution,
				),
			)
		}
	}
	switch params.SizeParam {
	case videoSizeConcrete:
		if tier == "" {
			// The size parameter has no default: the endpoint requires a
			// concrete resolution, so a tier has to be requested for the
			// declaration's size table to be consulted. The absent
			// resolution has no field of its own, so the rejection lands
			// on the resolution when the request named an unusable one
			// and on the video intent otherwise.
			target.reject(
				ledger,
				fmt.Sprintf(
					"%s takes a concrete size; request a resolution tier %s (optionally with a ratio) so it can be resolved",
					name, params.tierHint(),
				),
				inference.FieldGenerateIntentVideoResolution,
				inference.FieldGenerateIntentVideo,
			)
			break
		}
		// An unrequested ratio takes the platform's default: the size table
		// is keyed by ratio, so a tier alone still names one size.
		key := nonEmpty(ratio, defaultVideoRatio)
		size, ok := params.sizeFor(tier, key)
		if !ok {
			ledger.Reject(
				inference.FieldGenerateIntentVideoAspectRatio,
				fmt.Sprintf(
					"%s declares no size for tier %s and ratio %s; the declared pairs are %s",
					name, tier, key, params.sizePairs(),
				),
			)
			break
		}
		wire.resolution = size
		wire.sizeConcrete = true
	default:
		wire.resolution = tier
		if ratio != "" {
			wire.ratio = ratio
		}
	}
	if wire.smartDuration && video.DurationMillis != nil {
		// Two lengths in one request. The option is the more specific
		// request, so the intent's duration is the one that has to go.
		ledger.Reject(
			inference.FieldGenerateIntentVideoDuration,
			"the smart-duration option decides the length; drop duration_millis",
		)
	}
	if video.DurationMillis != nil {
		millis := *video.DurationMillis
		seconds := int(millis / 1000)
		switch {
		case millis%1000 != 0:
			ledger.Reject(
				inference.FieldGenerateIntentVideoDuration,
				fmt.Sprintf(
					"the Wan surfaces take whole seconds, not %d ms", millis,
				),
			)
		case !params.declaresDuration(seconds):
			ledger.Reject(
				inference.FieldGenerateIntentVideoDuration,
				params.durationReason(name, seconds),
			)
		default:
			wire.duration = &seconds
		}
	}
	wire.seed = video.Seed
	wire.watermark = video.Watermark
}

// compileVideoOptions lowers VideoOptions onto the wire, rejecting a knob the
// declaration does not enable.
func compileVideoOptions(
	wire *videoWire,
	params VideoParams,
	options VideoOptions,
	ledger *inference.Ledger,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.NegativePrompt != "" {
		if params.NegativePrompt {
			wire.negativePrompt = options.NegativePrompt
		} else {
			ledger.Reject(
				field("negative_prompt"),
				"this model does not declare a negative prompt",
			)
		}
	}
	if options.PromptExtend != nil {
		if (wire.hasEntry(mediaEntryFile) || options.Link != "") && !*options.PromptExtend {
			// The platform requires the rewrite while a document or a web
			// page rides the request, so turning it off is not a choice.
			ledger.Reject(
				field("prompt_extend"),
				"the platform requires the prompt rewrite while a file or web page rides the request",
			)
		}
		wire.promptExtend = options.PromptExtend
	}
	if options.Link != "" {
		switch {
		case params.Media == nil || !params.Media.Link:
			ledger.Reject(field("link"), "this model declares no web-page input")
		case wire.hasEntry(mediaEntryFile):
			ledger.Reject(
				field("link"),
				"a file and a web page cannot ride one request; the platform takes one document source",
			)
		default:
			wire.link = options.Link
		}
	}
	if options.SmartDuration {
		if params.SmartDuration {
			wire.smartDuration = true
		} else {
			ledger.Reject(
				field("smart_duration"),
				"this model does not declare the smart-duration request",
			)
		}
	}
	if options.ShotType != "" {
		if params.ShotType {
			wire.shotType = options.ShotType
		} else {
			ledger.Reject(
				field("shot_type"),
				"this model does not declare the shot-type knob",
			)
		}
	}
	if options.Audio != nil {
		if params.AudioSwitch {
			wire.audioSwitch = options.Audio
		} else {
			ledger.Reject(
				field("audio"),
				"this model does not declare an output-audio switch",
			)
		}
	}
	if options.Template != "" {
		if params.Template {
			wire.template = options.Template
		} else {
			ledger.Reject(
				field("template"),
				"this model does not declare video-effect templates",
			)
		}
	}
}

// videoRequest renders the task payload for the surface the wire selects.
func videoRequest(wire videoWire) map[string]any {
	input := map[string]any{}
	if wire.prompt != "" {
		input["prompt"] = wire.prompt
	}
	if wire.negativePrompt != "" {
		input["negative_prompt"] = wire.negativePrompt
	}
	if wire.imgURL != "" {
		input["img_url"] = wire.imgURL
	}
	if wire.firstFrame != "" {
		input["first_frame_url"] = wire.firstFrame
	}
	if wire.lastFrame != "" {
		input["last_frame_url"] = wire.lastFrame
	}
	if wire.audioURL != "" {
		input["audio_url"] = wire.audioURL
	}
	if wire.template != "" {
		input["template"] = wire.template
	}
	if len(wire.media) > 0 || wire.link != "" {
		input["media"] = mediaEntries(wire)
	}

	parameters := map[string]any{}
	if wire.resolution != "" {
		if wire.sizeConcrete {
			parameters["size"] = wire.resolution
		} else {
			parameters["resolution"] = wire.resolution
		}
	}
	if wire.ratio != "" {
		parameters["ratio"] = wire.ratio
	}
	switch {
	case wire.smartDuration:
		parameters["duration"] = smartDurationSeconds
	case wire.duration != nil:
		parameters["duration"] = *wire.duration
	}
	if wire.seed != nil {
		parameters["seed"] = *wire.seed
	}
	if wire.watermark != nil {
		parameters["watermark"] = *wire.watermark
	}
	if wire.promptExtend != nil {
		parameters["prompt_extend"] = *wire.promptExtend
	}
	if wire.shotType != "" {
		parameters["shot_type"] = wire.shotType
	}
	if wire.audioSwitch != nil {
		parameters["audio"] = *wire.audioSwitch
	}
	return map[string]any{
		"model":      wire.model,
		"input":      input,
		"parameters": parameters,
	}
}

// mediaEntries renders the input.media array: the entries the request parts
// produced, in the order the caller wrote them, then the web page the options
// carry. The platform numbers each type by its position, so the order is what
// the prompt's "图1"/"视频1"/"音频1" refers to.
func mediaEntries(wire videoWire) []map[string]any {
	entries := make([]map[string]any, 0, len(wire.media)+1)
	for _, entry := range wire.media {
		entries = append(entries, map[string]any{
			"type": entry.Type,
			"url":  entry.URL,
		})
	}
	if wire.link != "" {
		entries = append(entries, map[string]any{
			"type": mediaEntryLink,
			"url":  wire.link,
		})
	}
	return entries
}

func transportVideo(
	client *mediaClient,
	pollInterval time.Duration,
) inference.Transport[videoWire, videoRaw] {
	return func(ctx context.Context, wire videoWire) (videoRaw, error) {
		path := videoPathSynthesis
		if wire.keyframes {
			path = videoPathImage2
		}
		taskID, requestID, err := createTask(
			ctx, client, path, videoRequest(wire), "video generation")
		if err != nil {
			return videoRaw{}, err
		}
		query, err := awaitTask(ctx, client, pollInterval, taskID, "video generation")
		if err != nil {
			return videoRaw{}, err
		}
		if query.Output.VideoURL == "" {
			return videoRaw{}, fmt.Errorf(
				"dashscope: video generation returned no video url")
		}
		return videoRaw{
			requestID:  nonEmpty(query.RequestID, requestID),
			taskID:     taskID,
			url:        query.Output.VideoURL,
			videoCount: query.Usage.VideoCount,
		}, nil
	}
}

func decodeVideo(
	_ context.Context,
	raw videoRaw,
) (inference.GenerateResponse, error) {
	source, err := media.NewVideoURL(raw.url, videoMediaType)
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("dashscope: video url: %w", err)
	}
	generated := raw.videoCount
	if generated == 0 {
		generated = 1
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.VideoPart{Source: source},
			}},
		},
		FinishReason: inference.FinishCompleted,
		Usage:        inference.Usage{GeneratedVideos: &generated},
		Metadata: inference.Metadata{
			RequestID:  raw.requestID,
			ResponseID: raw.taskID,
		},
	}, nil
}

func openVideo(
	cls *clients,
	spec Spec,
	declared ModelSpec,
	id model.ModelID,
) (inference.GenerateOperations, error) {
	unary, err := inference.BindGenerate(
		compileVideo(declared, id.Name),
		transportVideo(cls.media, spec.pollInterval()),
		decodeVideo,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
