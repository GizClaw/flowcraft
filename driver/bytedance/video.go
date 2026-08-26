package bytedance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

// Video generation runs on the Ark content-generation task API (Seedance).
// The service is asynchronous: the transport creates a task, polls it to a
// terminal state, and folds the lifecycle into the unary contract. Context
// cancellation aborts the wait; the server-side task is left to expire via
// its TTL (VideoOptions.ExecutionExpiresAfter, provider default otherwise)
// rather than deleted, because the SDK delete call is best-effort and a
// cancelled caller no longer needs the artifact either way.
//
// Inputs map onto the content roles the task API understands: the first
// image is the first frame, the second is the last frame; any further
// images and any video/audio parts become reference inputs
// (reference_image / reference_video / reference_audio), which only the
// 2.0 series and 2.5 support. First/last-frame input and reference inputs
// are mutually exclusive, mirroring the official task scenarios.

type videoWire struct {
	model      string
	prompt     string
	firstFrame string
	lastFrame  string
	// Reference inputs (Seedance 2.0 series / 2.5 only).
	referenceImages []string
	referenceVideos []string
	referenceAudios []string
	duration        *int64 // seconds
	resolution      string // 480p | 720p | 1080p | 4k
	ratio           string // width:height, e.g. 16:9
	seed            *int64
	watermark       *bool
	// Extension settings (VideoOptions).
	cameraFixed           *bool
	generateAudio         *bool
	serviceTier           string
	executionExpiresAfter *int64
	priority              *int32
	outputFormat          string
	omniReferenceTaskType string
	webSearch             bool
	callbackURL           string
	safetyIdentifier      string
}

type videoRaw struct {
	videoURL         string
	completionTokens int64
}

// defaultVideoPollInterval paces task polls when the deployment Spec does
// not override it (Spec.video_poll_interval_millis). The official docs
// recommend polling no more often than every 10 seconds.
const defaultVideoPollInterval = 10 * time.Second

// statusExpired is the terminal state the task API reports when a task stays
// queued/running past execution_expires_after (official docs: 任务超时...
// 自动终止，并标记为 expired 状态). The pinned SDK defines no constant for
// it — content_generation.go lists only succeeded/cancelled/failed/running/
// queued — so the transport handles it locally.
const statusExpired = "expired"

func compileVideo(
	endpoint string,
	entry catalogEntry,
) inference.GenerateCompiler[videoWire] {
	return func(
		_ context.Context,
		model inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[videoWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := videoWire{
			model: endpoint,
		}
		if shape == inference.GenerateExecutionStream {
			ledger.reject(
				inference.FieldGenerateExecutionStream,
				"video generation is unary on this provider",
			)
		}

		var prompt []string
		var images []string
		var videos []string
		var audios []string
		collect := func(parts []message.Part, fields map[message.PartKind]inference.FieldID) {
			for _, part := range parts {
				switch value := part.(type) {
				case message.TextPart:
					prompt = append(prompt, value.Text)
				case message.ImagePart:
					images = append(images, sourceURI(value.Source))
				case message.VideoPart:
					videos = append(videos, value.Source.URL())
				case message.AudioPart:
					audios = append(audios, value.Source.URL())
				default:
					ledger.reject(
						fields[part.Kind()],
						fmt.Sprintf(
							"video generation accepts text, image, video, and audio parts, not %s",
							part.Kind(),
						),
					)
				}
			}
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				ledger.reject(
					inference.FieldGenerateContextRole,
					"video generation keeps user context only; assistant, system, and tool turns have no native channel",
				)
				continue
			}
			collect(turn.Content.Parts, contextPartFields)
		}
		collect(request.Input.Content.Parts, inputPartFields)
		wire.prompt = strings.Join(prompt, "\n")
		referenceMode := len(images) > 2 || len(videos) > 0 || len(audios) > 0
		switch {
		case len(images) == 1:
			if referenceMode {
				ledger.reject(
					inference.FieldGenerateInputImage,
					"first/last-frame input and reference inputs are mutually exclusive",
				)
			}
			wire.firstFrame = images[0]
		case len(images) == 2:
			if referenceMode {
				ledger.reject(
					inference.FieldGenerateInputImage,
					"first/last-frame input and reference inputs are mutually exclusive",
				)
			}
			wire.firstFrame, wire.lastFrame = images[0], images[1]
		case len(images) > 2:
			if entry.video.referenceImage == 0 {
				ledger.reject(
					inference.FieldGenerateInputImage,
					fmt.Sprintf(
						"model %s does not support reference-image input; it accepts at most a first-frame and a last-frame image",
						model.ID.Name,
					),
				)
			} else if len(images) > entry.video.referenceImage {
				ledger.reject(
					inference.FieldGenerateInputImage,
					fmt.Sprintf(
						"model %s supports at most %d reference images",
						model.ID.Name, entry.video.referenceImage,
					),
				)
			} else {
				wire.referenceImages = images
			}
		}
		switch {
		case len(videos) > 0 && entry.video.referenceVideo == 0:
			ledger.reject(
				inference.FieldGenerateInputVideo,
				fmt.Sprintf("model %s does not support video-reference input", model.ID.Name),
			)
		case len(videos) > entry.video.referenceVideo:
			ledger.reject(
				inference.FieldGenerateInputVideo,
				fmt.Sprintf(
					"model %s supports at most %d reference videos",
					model.ID.Name, entry.video.referenceVideo,
				),
			)
		default:
			wire.referenceVideos = videos
		}
		switch {
		case len(audios) > 0 && entry.video.referenceAudio == 0:
			ledger.reject(
				inference.FieldGenerateInputAudio,
				fmt.Sprintf("model %s does not support audio-reference input", model.ID.Name),
			)
		case len(audios) > entry.video.referenceAudio:
			ledger.reject(
				inference.FieldGenerateInputAudio,
				fmt.Sprintf(
					"model %s supports at most %d reference audio clips",
					model.ID.Name, entry.video.referenceAudio,
				),
			)
		default:
			wire.referenceAudios = audios
		}
		if len(audios) > 0 && len(images) == 0 && len(videos) == 0 &&
			!entry.video.audioOnly {
			ledger.reject(
				inference.FieldGenerateInputAudio,
				fmt.Sprintf(
					"model %s does not allow audio-only input; include at least one reference image or video",
					model.ID.Name,
				),
			)
		}

		intent := request.Input.Content.Intent
		if video := intent.Video; video != nil {
			if video.DurationMillis != nil {
				millis := *video.DurationMillis
				if millis%1000 != 0 {
					ledger.reject(
						inference.FieldGenerateIntentVideoDuration,
						"the task API bills whole seconds; sub-second durations cannot be honored",
					)
				} else {
					seconds := millis / 1000
					if min := entry.video.durationMin; min != nil && seconds < *min {
						ledger.reject(
							inference.FieldGenerateIntentVideoDuration,
							fmt.Sprintf("model %s requires a duration of at least %ds", model.ID.Name, *min),
						)
					}
					if max := entry.video.durationMax; max != nil && seconds > *max {
						ledger.reject(
							inference.FieldGenerateIntentVideoDuration,
							fmt.Sprintf("model %s caps duration at %ds", model.ID.Name, *max),
						)
					}
					wire.duration = &seconds
				}
			}
			if video.Resolution != "" {
				wire.resolution = strings.ToLower(video.Resolution)
				if cap := entry.maxResolution; cap != "" && !resolutionWithin(wire.resolution, cap) {
					ledger.reject(
						inference.FieldGenerateIntentVideoResolution,
						fmt.Sprintf("model %s caps resolution at %s", model.ID.Name, cap),
					)
				}
			}
			if video.AspectRatio != "" {
				wire.ratio = string(video.AspectRatio)
				if !validVideoRatio(wire.ratio) {
					ledger.reject(
						inference.FieldGenerateIntentVideoAspectRatio,
						fmt.Sprintf("unsupported video ratio %q", wire.ratio),
					)
				} else if entry.video.frameRatioAdaptiveOnly &&
					(wire.firstFrame != "" || wire.lastFrame != "") &&
					wire.ratio != "adaptive" {
					ledger.reject(
						inference.FieldGenerateIntentVideoAspectRatio,
						fmt.Sprintf(
							"model %s supports only ratio=adaptive for first/last-frame tasks",
							model.ID.Name,
						),
					)
				}
			}
			wire.seed = video.Seed
			if wire.seed != nil && !entry.video.seed {
				ledger.reject(
					inference.FieldGenerateIntentVideoSeed,
					fmt.Sprintf("model %s does not support seed", model.ID.Name),
				)
			} else if wire.seed != nil && (*wire.seed < -1 || *wire.seed > 2_147_483_647) {
				ledger.reject(
					inference.FieldGenerateIntentVideoSeed,
					"seed must be within [-1, 2147483647]",
				)
			}
			wire.watermark = video.Watermark
		}
		options, other := operationExtensions[VideoOptions](request.Extensions)
		rejectOtherExtensions("video generation", other, ledger)
		compileVideoOptions(&wire, options, entry, ledger, model.ID.Name)

		if text := intent.Text; text != nil {
			// Specific control rejections precede the wholesale text
			// rejection so the first failure names the offending field.
			rejectTextControls(text, ledger,
				"video models do not call tools",
				"the task API has no sampling controls",
				"video models have no thinking control",
			)
			ledger.reject(
				inference.FieldGenerateIntentText,
				"video models do not produce text",
			)
		}
		if intent.Image != nil {
			ledger.reject(
				inference.FieldGenerateIntentImage,
				"video models do not produce images",
			)
		}
		if intent.Audio != nil {
			ledger.reject(
				inference.FieldGenerateIntentAudio,
				"video models do not synthesize standalone audio; the generate_audio extension adds a track to the video",
			)
		}
		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[videoWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[videoWire]{Wire: wire, Report: report}, nil
	}
}

// compileVideoOptions lowers VideoOptions onto the wire and rejects
// extension settings the model does not support, per the official
// documentation's per-model support matrix (catalogEntry.video).
func compileVideoOptions(
	wire *videoWire,
	options VideoOptions,
	entry catalogEntry,
	ledger *ledger,
	modelName string,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.CameraFixed != nil && !entry.video.cameraFixed {
		ledger.reject(
			field("camera_fixed"),
			fmt.Sprintf("model %s does not support camera_fixed", modelName),
		)
	}
	if options.GenerateAudio != nil && !entry.video.generateAudio {
		ledger.reject(
			field("generate_audio"),
			fmt.Sprintf("model %s does not support generate_audio", modelName),
		)
	}
	if options.ServiceTier == "flex" && !entry.video.flexTier {
		ledger.reject(
			field("service_tier"),
			fmt.Sprintf("model %s does not support service_tier=flex", modelName),
		)
	}
	if options.Priority != nil && !entry.video.priority {
		ledger.reject(
			field("priority"),
			fmt.Sprintf("model %s does not support priority", modelName),
		)
	}
	if options.OutputFormat != nil && !entry.video.outputFormat {
		ledger.reject(
			field("output_format"),
			fmt.Sprintf("model %s does not support output_format", modelName),
		)
	}
	if options.OmniReferenceTaskType != nil && !entry.video.omniReference {
		ledger.reject(
			field("omni_reference_task_type"),
			fmt.Sprintf("model %s does not support omni_reference_task_type", modelName),
		)
	}
	if options.WebSearch != nil && *options.WebSearch &&
		!entry.capabilities.HostedWebSearch {
		ledger.reject(
			field("web_search"),
			fmt.Sprintf("model %s does not support web_search", modelName),
		)
	}
	if options.OmniReferenceTaskType != nil {
		taskType := *options.OmniReferenceTaskType
		if taskType == "edit" || taskType == "extend" {
			// Official constraints: at least one reference_video;
			// ratio=adaptive; edit additionally requires duration=-1.
			if len(wire.referenceVideos) == 0 {
				ledger.reject(
					field("omni_reference_task_type"),
					fmt.Sprintf("%s requires at least one reference video", taskType),
				)
			}
			if wire.ratio != "adaptive" {
				ledger.reject(
					field("omni_reference_task_type"),
					fmt.Sprintf("%s requires ratio=adaptive", taskType),
				)
			}
			if taskType == "edit" && wire.duration != nil {
				ledger.reject(
					field("omni_reference_task_type"),
					"edit requires duration=-1; omit the canonical duration",
				)
			}
		}
	}
	wire.cameraFixed = options.CameraFixed
	wire.generateAudio = options.GenerateAudio
	wire.serviceTier = options.ServiceTier
	wire.executionExpiresAfter = options.ExecutionExpiresAfter
	wire.priority = options.Priority
	wire.outputFormat = derefString(options.OutputFormat)
	wire.omniReferenceTaskType = derefString(options.OmniReferenceTaskType)
	wire.webSearch = options.WebSearch != nil && *options.WebSearch
	wire.callbackURL = derefString(options.CallbackURL)
	wire.safetyIdentifier = derefString(options.SafetyIdentifier)
}

// validVideoRatio reports whether ratio is one of the official create-task
// API values (16:9 / 4:3 / 1:1 / 3:4 / 9:16 / 21:9 / adaptive).
func validVideoRatio(ratio string) bool {
	switch ratio {
	case "16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive":
		return true
	}
	return false
}

// resolutionWithin reports whether resolution fits inside the model's
// resolution cap. Tiers order linearly: Np tiers by their line count, Nk
// tiers by 540 lines per K (4k ≈ 2160p).
func resolutionWithin(resolution, cap string) bool {
	tier := func(token string) (int, bool) {
		lower := strings.ToLower(token)
		if lines, ok := strings.CutSuffix(lower, "p"); ok {
			value, err := strconv.Atoi(lines)
			return value, err == nil
		}
		if ks, ok := strings.CutSuffix(lower, "k"); ok {
			value, err := strconv.Atoi(ks)
			return value * 540, err == nil
		}
		return 0, false
	}
	resolutionTier, ok := tier(resolution)
	if !ok {
		return false
	}
	capTier, ok := tier(cap)
	return ok && resolutionTier <= capTier
}

func transportVideo(
	client *arkruntime.Client,
	pollInterval time.Duration,
) inference.Transport[videoWire, videoRaw] {
	return func(ctx context.Context, wire videoWire) (videoRaw, error) {
		prompt := wire.prompt
		content := []*arkmodel.CreateContentGenerationContentItem{{
			Type: arkmodel.ContentGenerationContentItemTypeText,
			Text: &prompt,
		}}
		if wire.firstFrame != "" {
			content = append(content, itemImage(wire.firstFrame, "first_frame"))
		}
		if wire.lastFrame != "" {
			content = append(content, itemImage(wire.lastFrame, "last_frame"))
		}
		for _, url := range wire.referenceImages {
			content = append(content, itemImage(url, "reference_image"))
		}
		for _, url := range wire.referenceVideos {
			content = append(content, itemVideo(url, "reference_video"))
		}
		for _, url := range wire.referenceAudios {
			content = append(content, itemAudio(url, "reference_audio"))
		}
		request := arkmodel.CreateContentGenerationTaskRequest{
			Model:                 wire.model,
			Content:               content,
			Duration:              wire.duration,
			Seed:                  wire.seed,
			Watermark:             wire.watermark,
			CameraFixed:           wire.cameraFixed,
			GenerateAudio:         wire.generateAudio,
			ExecutionExpiresAfter: wire.executionExpiresAfter,
		}
		if wire.resolution != "" {
			request.Resolution = &wire.resolution
		}
		if wire.ratio != "" {
			request.Ratio = &wire.ratio
		}
		if wire.serviceTier != "" {
			request.ServiceTier = &wire.serviceTier
		}
		request.Priority = wire.priority
		if wire.outputFormat != "" {
			request.OutputFormat = &wire.outputFormat
		}
		if wire.omniReferenceTaskType != "" {
			request.OmniReferenceTaskType = &wire.omniReferenceTaskType
		}
		if wire.webSearch {
			request.Tools = []*arkmodel.ContentGenerationTool{{
				Type: arkmodel.ToolTypeWebSearch,
			}}
		}
		if wire.callbackURL != "" {
			request.CallbackUrl = &wire.callbackURL
		}
		if wire.safetyIdentifier != "" {
			request.SafetyIdentifier = &wire.safetyIdentifier
		}
		created, err := client.CreateContentGenerationTask(ctx, request)
		if err != nil {
			return videoRaw{}, classifyError(err)
		}
		for {
			task, err := client.GetContentGenerationTask(
				ctx,
				arkmodel.GetContentGenerationTaskRequest{ID: created.ID},
			)
			if err != nil {
				return videoRaw{}, classifyError(err)
			}
			switch task.Status {
			case arkmodel.StatusSucceeded:
				if task.Content.VideoURL == "" {
					return videoRaw{}, fmt.Errorf(
						"bytedance: succeeded video task %q carries no video url",
						task.ID,
					)
				}
				return videoRaw{
					videoURL:         task.Content.VideoURL,
					completionTokens: int64(task.Usage.CompletionTokens),
				}, nil
			case arkmodel.StatusFailed:
				code, message := "", ""
				if task.Error != nil {
					code, message = task.Error.Code, task.Error.Message
				}
				return videoRaw{}, classifyResponseError(code, message)
			case arkmodel.StatusCancelled:
				return videoRaw{}, errdefs.NotAvailable(fmt.Errorf(
					"bytedance: video task %q was cancelled server-side",
					task.ID,
				))
			case statusExpired:
				return videoRaw{}, errdefs.Timeout(fmt.Errorf(
					"bytedance: video task %q expired server-side",
					task.ID,
				))
			}
			select {
			case <-ctx.Done():
				return videoRaw{}, ctx.Err()
			case <-time.After(pollInterval):
			}
		}
	}
}

// itemImage / itemVideo / itemAudio build the official content items with
// their role. The roles are fixed by the compiler: first/last frame for
// bookend images, reference_* for every other input.
func itemImage(url, role string) *arkmodel.CreateContentGenerationContentItem {
	return &arkmodel.CreateContentGenerationContentItem{
		Type:     arkmodel.ContentGenerationContentItemTypeImage,
		ImageURL: &arkmodel.ImageURL{URL: url},
		Role:     &role,
	}
}

func itemVideo(url, role string) *arkmodel.CreateContentGenerationContentItem {
	return &arkmodel.CreateContentGenerationContentItem{
		Type:     arkmodel.ContentGenerationContentItemTypeVideo,
		VideoURL: &arkmodel.VideoUrl{Url: url},
		Role:     &role,
	}
}

func itemAudio(url, role string) *arkmodel.CreateContentGenerationContentItem {
	return &arkmodel.CreateContentGenerationContentItem{
		Type:     arkmodel.ContentGenerationContentItemTypeAudio,
		AudioURL: &arkmodel.AudioUrl{Url: url},
		Role:     &role,
	}
}

func decodeVideo(
	_ context.Context,
	raw videoRaw,
) (inference.GenerateResponse, error) {
	// Seedance tasks deliver mp4 files; the URL carries no explicit media
	// type, so the compiler-known container is the truthful one.
	source, err := media.NewVideoURL(raw.videoURL, "video/mp4")
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("bytedance: video url: %w", err)
	}
	generated := int64(1)
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{message.VideoPart{Source: source}}},
		},
		FinishReason: inference.FinishCompleted,
		Usage: inference.Usage{
			OutputTokens:    raw.completionTokens,
			TotalTokens:     raw.completionTokens,
			GeneratedVideos: &generated,
		},
	}, nil
}

func openVideo(
	cls *clients,
	spec Spec,
	entry catalogEntry,
	id inference.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	ark, err := cls.requireArk(profile)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	unary, err := inference.BindGenerate(
		compileVideo(cls.endpoint(id.Name), entry),
		transportVideo(ark, spec.videoPollInterval()),
		decodeVideo,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
