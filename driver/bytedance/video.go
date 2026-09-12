package bytedance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/utils/ptr"

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
) inference.GenerateCompiler[*arkmodel.CreateContentGenerationTaskRequest] {
	return func(
		_ context.Context,
		ref model.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[*arkmodel.CreateContentGenerationTaskRequest], error) {
		ledger := inference.NewLedger(
			model.OperationGenerate,
			providerID,
			request.ActiveFieldsFor(shape),
		)
		ark := &arkmodel.CreateContentGenerationTaskRequest{Model: endpoint}
		if shape == inference.GenerateExecutionStream {
			ledger.Reject(
				inference.FieldGenerateExecutionStream,
				"video generation is unary on this provider",
			)
		}

		var prompt []string
		var images []string
		var videos []string
		var audios []string
		collect := func(parts []message.Part, fields func(message.PartKind) inference.FieldID) {
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
					ledger.Reject(
						fields(part.Kind()),
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
				ledger.Reject(
					inference.FieldGenerateContextRole,
					"video generation keeps user context only; assistant, system, and tool turns have no native channel",
				)
				continue
			}
			collect(turn.Content.Parts, contextPartField)
		}
		collect(request.Input.Content.Parts, inputPartField)
		joined := strings.Join(prompt, "\n")
		ark.Content = []*arkmodel.CreateContentGenerationContentItem{{
			Type: arkmodel.ContentGenerationContentItemTypeText,
			Text: &joined,
		}}

		// The image count picks the role every input plays: one image is the
		// first frame, two are the bookends, and anything beyond that is a
		// reference input the 2.0 series and 2.5 accept.
		referenceMode := len(images) > 2 || len(videos) > 0 || len(audios) > 0
		switch {
		case len(images) == 1:
			if referenceMode {
				ledger.Reject(
					inference.FieldGenerateInputImage,
					"first/last-frame input and reference inputs are mutually exclusive",
				)
			}
			ark.Content = append(ark.Content, itemImage(images[0], "first_frame"))
		case len(images) == 2:
			if referenceMode {
				ledger.Reject(
					inference.FieldGenerateInputImage,
					"first/last-frame input and reference inputs are mutually exclusive",
				)
			}
			ark.Content = append(ark.Content,
				itemImage(images[0], "first_frame"),
				itemImage(images[1], "last_frame"),
			)
		case len(images) > 2:
			if entry.video.referenceImage == 0 {
				ledger.Reject(
					inference.FieldGenerateInputImage,
					fmt.Sprintf(
						"model %s does not support reference-image input; it accepts at most a first-frame and a last-frame image",
						ref.ID.Name,
					),
				)
			} else if len(images) > entry.video.referenceImage {
				ledger.Reject(
					inference.FieldGenerateInputImage,
					fmt.Sprintf(
						"model %s supports at most %d reference images",
						ref.ID.Name, entry.video.referenceImage,
					),
				)
			} else {
				for _, url := range images {
					ark.Content = append(ark.Content, itemImage(url, "reference_image"))
				}
			}
		}
		switch {
		case len(videos) > 0 && entry.video.referenceVideo == 0:
			ledger.Reject(
				inference.FieldGenerateInputVideo,
				fmt.Sprintf("model %s does not support video-reference input", ref.ID.Name),
			)
		case len(videos) > entry.video.referenceVideo:
			ledger.Reject(
				inference.FieldGenerateInputVideo,
				fmt.Sprintf(
					"model %s supports at most %d reference videos",
					ref.ID.Name, entry.video.referenceVideo,
				),
			)
		default:
			for _, url := range videos {
				ark.Content = append(ark.Content, itemVideo(url, "reference_video"))
			}
		}
		switch {
		case len(audios) > 0 && entry.video.referenceAudio == 0:
			ledger.Reject(
				inference.FieldGenerateInputAudio,
				fmt.Sprintf("model %s does not support audio-reference input", ref.ID.Name),
			)
		case len(audios) > entry.video.referenceAudio:
			ledger.Reject(
				inference.FieldGenerateInputAudio,
				fmt.Sprintf(
					"model %s supports at most %d reference audio clips",
					ref.ID.Name, entry.video.referenceAudio,
				),
			)
		default:
			for _, url := range audios {
				ark.Content = append(ark.Content, itemAudio(url, "reference_audio"))
			}
		}
		if len(audios) > 0 && len(images) == 0 && len(videos) == 0 &&
			!entry.video.audioOnly {
			ledger.Reject(
				inference.FieldGenerateInputAudio,
				fmt.Sprintf(
					"model %s does not allow audio-only input; include at least one reference image or video",
					ref.ID.Name,
				),
			)
		}

		intent := request.Input.Content.Intent
		if video := intent.Video; video != nil {
			if video.DurationMillis != nil {
				millis := *video.DurationMillis
				if millis%1000 != 0 {
					ledger.Reject(
						inference.FieldGenerateIntentVideoDuration,
						"the task API bills whole seconds; sub-second durations cannot be honored",
					)
				} else {
					seconds := millis / 1000
					if min := entry.video.durationMin; min != nil && seconds < *min {
						ledger.Reject(
							inference.FieldGenerateIntentVideoDuration,
							fmt.Sprintf("model %s requires a duration of at least %ds", ref.ID.Name, *min),
						)
					}
					if max := entry.video.durationMax; max != nil && seconds > *max {
						ledger.Reject(
							inference.FieldGenerateIntentVideoDuration,
							fmt.Sprintf("model %s caps duration at %ds", ref.ID.Name, *max),
						)
					}
					ark.Duration = &seconds
				}
			}
			if video.Resolution != "" {
				resolution := strings.ToLower(video.Resolution)
				ark.Resolution = &resolution
				if cap := entry.maxResolution; cap != "" && !resolutionWithin(resolution, cap) {
					ledger.Reject(
						inference.FieldGenerateIntentVideoResolution,
						fmt.Sprintf("model %s caps resolution at %s", ref.ID.Name, cap),
					)
				}
			}
			if video.AspectRatio != "" {
				ratio := string(video.AspectRatio)
				ark.Ratio = &ratio
				if !validVideoRatio(ratio) {
					ledger.Reject(
						inference.FieldGenerateIntentVideoAspectRatio,
						fmt.Sprintf("unsupported video ratio %q", ratio),
					)
				} else if entry.video.frameRatioAdaptiveOnly &&
					(hasContentRole(ark, "first_frame") ||
						hasContentRole(ark, "last_frame")) &&
					ratio != "adaptive" {
					ledger.Reject(
						inference.FieldGenerateIntentVideoAspectRatio,
						fmt.Sprintf(
							"model %s supports only ratio=adaptive for first/last-frame tasks",
							ref.ID.Name,
						),
					)
				}
			}
			ark.Seed = ptr.Clone(video.Seed)
			if ark.Seed != nil && !entry.video.seed {
				ledger.Reject(
					inference.FieldGenerateIntentVideoSeed,
					fmt.Sprintf("model %s does not support seed", ref.ID.Name),
				)
			} else if ark.Seed != nil && (*ark.Seed < -1 || *ark.Seed > 2_147_483_647) {
				ledger.Reject(
					inference.FieldGenerateIntentVideoSeed,
					"seed must be within [-1, 2147483647]",
				)
			}
			ark.Watermark = ptr.Clone(video.Watermark)
		}
		options, other := inference.ExtensionFor[VideoOptions](request.Extensions)
		ledger.RejectExtensions("video generation", other)
		compileVideoOptions(ark, options, entry, ledger, ref.ID.Name)

		if text := intent.Text; text != nil {
			// Specific control rejections precede the wholesale text
			// rejection so the first failure names the offending field.
			rejectTextControls(text, ledger,
				"video models do not call tools",
				"the task API has no sampling controls",
				"video models have no thinking control",
			)
			ledger.Reject(
				inference.FieldGenerateIntentText,
				"video models do not produce text",
			)
		}
		if intent.Image != nil {
			ledger.Reject(
				inference.FieldGenerateIntentImage,
				"video models do not produce images",
			)
		}
		if intent.Audio != nil {
			ledger.Reject(
				inference.FieldGenerateIntentAudio,
				"video models do not synthesize standalone audio; the generate_audio extension adds a track to the video",
			)
		}
		report := ledger.Report()
		if ledger.Rejected() {
			return inference.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{
				Report: report,
			}, ledger.Err()
		}
		return inference.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{
			Wire:   ark,
			Report: report,
		}, nil
	}
}

// compileVideoOptions lowers VideoOptions onto the task request and rejects
// extension settings the model does not support, per the official
// documentation's per-model support matrix (catalogEntry.video).
func compileVideoOptions(
	ark *arkmodel.CreateContentGenerationTaskRequest,
	options VideoOptions,
	entry catalogEntry,
	ledger *inference.Ledger,
	modelName string,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.CameraFixed != nil && !entry.video.cameraFixed {
		ledger.Reject(
			field("camera_fixed"),
			fmt.Sprintf("model %s does not support camera_fixed", modelName),
		)
	}
	if options.GenerateAudio != nil && !entry.video.generateAudio {
		ledger.Reject(
			field("generate_audio"),
			fmt.Sprintf("model %s does not support generate_audio", modelName),
		)
	}
	if options.ServiceTier == "flex" && !entry.video.flexTier {
		ledger.Reject(
			field("service_tier"),
			fmt.Sprintf("model %s does not support service_tier=flex", modelName),
		)
	}
	if options.Priority != nil && !entry.video.priority {
		ledger.Reject(
			field("priority"),
			fmt.Sprintf("model %s does not support priority", modelName),
		)
	}
	if options.OutputFormat != nil && !entry.video.outputFormat {
		ledger.Reject(
			field("output_format"),
			fmt.Sprintf("model %s does not support output_format", modelName),
		)
	}
	if options.OmniReferenceTaskType != nil && !entry.video.omniReference {
		ledger.Reject(
			field("omni_reference_task_type"),
			fmt.Sprintf("model %s does not support omni_reference_task_type", modelName),
		)
	}
	if options.WebSearch != nil && *options.WebSearch &&
		!entry.capabilities.HostedWebSearch {
		ledger.Reject(
			field("web_search"),
			fmt.Sprintf("model %s does not support web_search", modelName),
		)
	}
	if options.OmniReferenceTaskType != nil {
		taskType := *options.OmniReferenceTaskType
		if taskType == "edit" || taskType == "extend" {
			// Official constraints: at least one reference_video;
			// ratio=adaptive; edit additionally requires duration=-1.
			if !hasContentRole(ark, "reference_video") {
				ledger.Reject(
					field("omni_reference_task_type"),
					fmt.Sprintf("%s requires at least one reference video", taskType),
				)
			}
			if derefString(ark.Ratio) != "adaptive" {
				ledger.Reject(
					field("omni_reference_task_type"),
					fmt.Sprintf("%s requires ratio=adaptive", taskType),
				)
			}
			if taskType == "edit" && ark.Duration != nil {
				ledger.Reject(
					field("omni_reference_task_type"),
					"edit requires duration=-1; omit the canonical duration",
				)
			}
		}
	}
	ark.CameraFixed = ptr.Clone(options.CameraFixed)
	ark.GenerateAudio = ptr.Clone(options.GenerateAudio)
	if options.ServiceTier != "" {
		tier := options.ServiceTier
		ark.ServiceTier = &tier
	}
	ark.ExecutionExpiresAfter = ptr.Clone(options.ExecutionExpiresAfter)
	ark.Priority = ptr.Clone(options.Priority)
	if format := derefString(options.OutputFormat); format != "" {
		ark.OutputFormat = &format
	}
	if taskType := derefString(options.OmniReferenceTaskType); taskType != "" {
		ark.OmniReferenceTaskType = &taskType
	}
	if options.WebSearch != nil && *options.WebSearch {
		ark.Tools = []*arkmodel.ContentGenerationTool{{
			Type: arkmodel.ToolTypeWebSearch,
		}}
	}
	if url := derefString(options.CallbackURL); url != "" {
		ark.CallbackUrl = &url
	}
	if identifier := derefString(options.SafetyIdentifier); identifier != "" {
		ark.SafetyIdentifier = &identifier
	}
}

// hasContentRole reports whether the compiled content carries an item with the
// given role.
func hasContentRole(
	ark *arkmodel.CreateContentGenerationTaskRequest,
	role string,
) bool {
	for _, item := range ark.Content {
		if item.Role != nil && *item.Role == role {
			return true
		}
	}
	return false
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
	options []arkruntime.RequestOption,
) inference.Transport[*arkmodel.CreateContentGenerationTaskRequest, videoRaw] {
	return func(
		ctx context.Context,
		request *arkmodel.CreateContentGenerationTaskRequest,
	) (videoRaw, error) {
		created, err := client.CreateContentGenerationTask(ctx, *request, options...)
		if err != nil {
			return videoRaw{}, classifyError(err)
		}
		for {
			task, err := client.GetContentGenerationTask(
				ctx,
				arkmodel.GetContentGenerationTaskRequest{ID: created.ID},
				options...,
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
	id model.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	ark, err := cls.requireArk(profile)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	unary, err := inference.BindGenerate(
		compileVideo(cls.endpoint(id.Name), entry),
		transportVideo(ark, spec.videoPollInterval(), cls.arkRequestOptions),
		decodeVideo,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
