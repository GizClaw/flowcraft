package minimax

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Video generation runs on MiniMax's async task pipeline: create the task,
// poll its status, then surface the finished artifact's URL. The download
// URL expires one hour after retrieval, so the response carries it verbatim
// — downloading and persisting is the caller's job.
//
// Two task APIs exist:
//
//   - v1 (/v1/video_generation) serves the Hailuo 2.3/2.3-Fast/02 trio with
//     flat fields: prompt, first/last-frame images, duration (6s, or 10s at
//     768P), resolution (768P/1080P; 512P for 02 image-to-video),
//     aigc_watermark, and the prompt_optimizer / fast_pretreatment /
//     callback_url knobs. There is no aspect-ratio or seed control, so
//     those intents reject. Hailuo-2.3-Fast is image-to-video only.
//
//   - v2 (/v2/video_generation) serves MiniMax-H3 with a multimodal content
//     array (text/image/video/audio items carrying first_frame/last_frame/
//     reference_* roles), a required resolution (768P/2K) and duration
//     (4-15s), an optional ratio (text-only tasks default to 16:9 and
//     reject adaptive; image tasks default to adaptive server-side), and
//     aigc_watermark/callback_url. Its query endpoint returns the artifact
//     URL directly, so there is no file-retrieval hop. H3-Context-IR
//     (context_ir.go) rides the same query endpoint and shares the content
//     role mapping and poll loop.
//
// Inline media become data URIs carrying their source media type; absolute
// URLs pass through verbatim.

// v2Content holds the multimodal input roles both v2 task APIs understand:
// first/last-frame images and reference image/video/audio items. The v2
// video and H3-Context-IR compilers share it so the content mapping and
// its limits stay in one place.
type v2Content struct {
	firstFrame      string
	lastFrame       string
	referenceImages []string
	referenceVideos []string
	referenceAudios []string
}

type videoWire struct {
	model  string
	prompt string
	v2Content
	duration         *int // seconds; nil = provider default (v1)
	resolution       string
	ratio            string
	watermark        *bool
	promptOptimizer  *bool // v1 only
	fastPretreatment *bool // v1 only
	callbackURL      string
}

type videoRaw struct {
	videoURL  string
	requestID string
}

// v2RatioValues are the official v2 ratio values. adaptive is the server
// default for image/reference tasks and invalid for text-only tasks.
var v2RatioValues = map[string]bool{
	"adaptive": true, "21:9": true, "16:9": true, "4:3": true,
	"1:1": true, "3:4": true, "9:16": true,
}

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
		wire := videoWire{model: endpoint}
		if shape == inference.GenerateExecutionStream {
			ledger.reject(
				inference.FieldGenerateExecutionStream,
				"video generation is a unary task on this provider",
			)
		}

		prompt, images, videos, audios := collectTaskParts(request, ledger)
		wire.prompt = strings.Join(prompt, "\n")

		compileVideoInputs(&wire, images, videos, audios, entry, model, ledger)
		limit, label := 2000, "minimax v1 prompts"
		if entry.videoV2 {
			limit, label = 7000, "MiniMax-H3 prompts"
		}
		rejectPromptLength(
			request,
			ledger,
			len(wire.prompt),
			limit,
			fmt.Sprintf("%s are at most %d characters, not %d", label, limit, len(wire.prompt)),
		)
		if entry.videoV2 && wire.prompt == "" {
			// The v2 schema requires a non-empty text item in content.
			reason := fmt.Sprintf("%s requires a non-empty text prompt", endpoint)
			if len(prompt) > 0 {
				ledger.reject(inference.FieldGenerateInputText, reason)
			} else if intent := request.Input.Content.Intent; intent.Video != nil {
				ledger.reject(inference.FieldGenerateIntentVideo, reason)
			} else {
				ledger.reject(inference.FieldGenerateInputRole, reason)
			}
		}
		if entry.videoI2VOnly && wire.firstFrame == "" {
			// A missing first frame is a required-input absence, so no part
			// field is active to reject; the rejection lands on the closest
			// active field — the video intent when present, the input role
			// otherwise.
			reason := fmt.Sprintf(
				"%s is image-to-video only; the input must carry a first-frame image",
				endpoint,
			)
			if intent := request.Input.Content.Intent; intent.Video != nil {
				ledger.reject(inference.FieldGenerateIntentVideo, reason)
			} else {
				ledger.reject(inference.FieldGenerateInputRole, reason)
			}
		}

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
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
				"video models do not synthesize standalone audio",
			)
		}
		if video := intent.Video; video != nil {
			compileVideoIntent(&wire, video, entry, endpoint, ledger)
		}
		options, other := operationExtensions[VideoOptions](request.Extensions)
		compileVideoOptions(&wire, options, entry, endpoint, ledger)
		rejectOtherExtensions("video generation", other, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[videoWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[videoWire]{Wire: wire, Report: report}, nil
	}
}

// collectTaskParts gathers text/image/video/audio parts from the user
// context turns and the request input, rendering media the way the task
// APIs expect them (URL passthrough or data URIs). Non-media parts reject.
// Both the video and H3-Context-IR compilers share it.
func collectTaskParts(
	request inference.GenerateRequest,
	ledger *ledger,
) (prompt []string, images, videos, audios []string) {
	collect := func(parts []message.Part, fields map[message.PartKind]inference.FieldID) {
		for _, part := range parts {
			switch value := part.(type) {
			case message.TextPart:
				prompt = append(prompt, value.Text)
			case message.ImagePart:
				if value.Source.Kind() == media.SourceStream {
					ledger.reject(
						fields[message.PartImage],
						"stream media sources must be materialized before generate",
					)
					continue
				}
				images = append(images, videoImageValue(value.Source))
			case message.VideoPart:
				if value.Source.Kind() == media.SourceStream {
					ledger.reject(
						fields[message.PartVideo],
						"stream media sources must be materialized before generate",
					)
					continue
				}
				videos = append(videos, videoSourceURI(value.Source))
			case message.AudioPart:
				if value.Source.Kind() == media.SourceStream {
					ledger.reject(
						fields[message.PartAudio],
						"stream media sources must be materialized before generate",
					)
					continue
				}
				audios = append(audios, audioSourceURI(value.Source))
			default:
				ledger.reject(
					fields[part.Kind()],
					fmt.Sprintf(
						"the task accepts text, image, video, and audio parts, not %s",
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
				"task generation keeps user context only; assistant, system, and tool turns have no native channel",
			)
			continue
		}
		collect(turn.Content.Parts, contextPartFields)
	}
	collect(request.Input.Content.Parts, inputPartFields)
	return
}

// rejectPromptLength rejects a prompt that exceeds the official character
// limit, landing on the most precise active text field: the input text
// when the request input carries text, otherwise a context text field,
// otherwise the input role.
func rejectPromptLength(
	request inference.GenerateRequest,
	ledger *ledger,
	length, limit int,
	reason string,
) {
	if length <= limit {
		return
	}
	if containsTextPart(request.Input.Content.Parts) {
		ledger.reject(inference.FieldGenerateInputText, reason)
		return
	}
	for _, turn := range request.Context {
		if containsTextPart(turn.Content.Parts) {
			ledger.reject(inference.FieldGenerateContextText, reason)
			return
		}
	}
	ledger.reject(inference.FieldGenerateInputRole, reason)
}

func containsTextPart(parts []message.Part) bool {
	for _, part := range parts {
		if part != nil && part.Kind() == message.PartText {
			return true
		}
	}
	return false
}

// compileVideoInputs lowers the collected media parts onto the wire. The
// v2 API understands every input role (first/last frame and reference_*);
// v1 understands first/last-frame images only, on the models that declare
// the capability.
func compileVideoInputs(
	wire *videoWire,
	images, videos, audios []string,
	entry catalogEntry,
	model inference.ModelRef,
	ledger *ledger,
) {
	if entry.videoV2 {
		compileV2Inputs(&wire.v2Content, images, videos, audios, ledger)
		return
	}
	switch {
	case len(images) == 1:
		wire.firstFrame = images[0]
	case len(images) == 2 && entry.videoLastFrame:
		wire.firstFrame, wire.lastFrame = images[0], images[1]
	case len(images) > 0:
		if entry.videoLastFrame {
			ledger.reject(
				inference.FieldGenerateInputImage,
				fmt.Sprintf(
					"%s accepts a first-frame and a last-frame image, not %d",
					model.ID.Name, len(images),
				),
			)
		} else {
			ledger.reject(
				inference.FieldGenerateInputImage,
				fmt.Sprintf(
					"%s accepts a single first-frame image, not %d",
					model.ID.Name, len(images),
				),
			)
		}
	}
	if len(videos) > 0 {
		ledger.reject(
			inference.FieldGenerateInputVideo,
			fmt.Sprintf("%s does not accept reference-video input", model.ID.Name),
		)
	}
	if len(audios) > 0 {
		ledger.reject(
			inference.FieldGenerateInputAudio,
			fmt.Sprintf("%s does not accept reference-audio input", model.ID.Name),
		)
	}
}

// compileV2Inputs maps media parts onto the v2 content roles. One image is
// the first frame, two are first/last frames, and more than two — or any
// video/audio part — switch the task to multimodal reference generation;
// the two scenarios are mutually exclusive, mirroring the official content
// rules.
func compileV2Inputs(
	wire *v2Content,
	images, videos, audios []string,
	ledger *ledger,
) {
	firstLast := len(images) == 1 || len(images) == 2
	reference := len(images) > 2 || len(videos) > 0 || len(audios) > 0
	if firstLast && reference {
		ledger.reject(
			inference.FieldGenerateInputImage,
			"MiniMax-H3 treats first/last-frame input and reference inputs as mutually exclusive",
		)
		return
	}
	switch {
	case len(images) == 1:
		wire.firstFrame = images[0]
	case len(images) == 2:
		wire.firstFrame, wire.lastFrame = images[0], images[1]
	case len(images) > 9:
		ledger.reject(
			inference.FieldGenerateInputImage,
			"MiniMax-H3 accepts at most 9 reference images",
		)
	case len(images) > 2:
		wire.referenceImages = images
	}
	if len(videos) > 3 {
		ledger.reject(
			inference.FieldGenerateInputVideo,
			"MiniMax-H3 accepts at most 3 reference videos",
		)
	} else {
		wire.referenceVideos = videos
	}
	if len(audios) > 3 {
		ledger.reject(
			inference.FieldGenerateInputAudio,
			"MiniMax-H3 accepts at most 3 reference audio clips",
		)
	} else {
		wire.referenceAudios = audios
	}
}

// compileVideoIntent lowers VideoIntent controls onto the wire and enforces
// the official per-model constraints.
func compileVideoIntent(
	wire *videoWire,
	video *inference.VideoIntent,
	entry catalogEntry,
	endpoint string,
	ledger *ledger,
) {
	if video.DurationMillis != nil {
		millis := *video.DurationMillis
		if millis%1000 != 0 {
			ledger.reject(
				inference.FieldGenerateIntentVideoDuration,
				fmt.Sprintf("minimax video durations are whole seconds, not %dms", millis),
			)
		} else {
			seconds := int(millis / 1000)
			if entry.videoV2 {
				if seconds < 4 || seconds > 15 {
					ledger.reject(
						inference.FieldGenerateIntentVideoDuration,
						fmt.Sprintf("MiniMax-H3 durations are 4-15s, not %ds", seconds),
					)
				} else {
					wire.duration = &seconds
				}
			} else {
				switch {
				case seconds == 6:
					wire.duration = &seconds
				case seconds == 10 && !entry.video10s:
					ledger.reject(
						inference.FieldGenerateIntentVideoDuration,
						fmt.Sprintf("%s serves 6-second videos only", endpoint),
					)
				case seconds == 10:
					wire.duration = &seconds
				default:
					ledger.reject(
						inference.FieldGenerateIntentVideoDuration,
						fmt.Sprintf("minimax video durations are 6s or 10s, not %ds", seconds),
					)
				}
			}
		}
	} else if entry.videoV2 {
		// The v2 schema requires duration; the driver defaults to the same
		// 6s the v1 API uses when the intent leaves it open.
		seconds := 6
		wire.duration = &seconds
	}

	if video.Resolution != "" {
		switch strings.ToUpper(video.Resolution) {
		case "768P":
			wire.resolution = "768P"
		case "2K":
			if !entry.videoV2 {
				ledger.reject(
					inference.FieldGenerateIntentVideoResolution,
					fmt.Sprintf("%s serves 768P/1080P tiers, not %q", endpoint, video.Resolution),
				)
			} else {
				wire.resolution = "2K"
			}
		case "1080P":
			if entry.videoV2 {
				ledger.reject(
					inference.FieldGenerateIntentVideoResolution,
					fmt.Sprintf("%s serves 768P/2K tiers, not %q", endpoint, video.Resolution),
				)
			} else if !entry.videoHD {
				ledger.reject(
					inference.FieldGenerateIntentVideoResolution,
					fmt.Sprintf("%s serves 768P only", endpoint),
				)
			} else {
				wire.resolution = "1080P"
			}
		case "512P":
			// 512P exists on the v1 image-to-video surface for
			// MiniMax-Hailuo-02 only: single-first-frame tasks only.
			// Text-to-video and first/last-frame (fl2v) tasks cap at
			// 768P/1080P per the official docs.
			if !entry.video512P || wire.firstFrame == "" || wire.lastFrame != "" {
				ledger.reject(
					inference.FieldGenerateIntentVideoResolution,
					fmt.Sprintf("%s serves 768P/1080P tiers, not %q", endpoint, video.Resolution),
				)
			} else {
				wire.resolution = "512P"
			}
		default:
			tiers := "768P/1080P"
			if entry.videoV2 {
				tiers = "768P/2K"
			}
			ledger.reject(
				inference.FieldGenerateIntentVideoResolution,
				fmt.Sprintf("%s serves %s tiers, not %q", endpoint, tiers, video.Resolution),
			)
		}
	} else if entry.videoV2 {
		// The v2 schema requires resolution; 768P is the default tier.
		wire.resolution = "768P"
	}
	if !entry.videoV2 && wire.duration != nil && *wire.duration == 10 &&
		wire.resolution == "1080P" {
		ledger.reject(
			inference.FieldGenerateIntentVideoDuration,
			"10-second videos require 768P",
		)
	}

	if video.AspectRatio != "" {
		if !entry.videoV2 {
			ledger.reject(
				inference.FieldGenerateIntentVideoAspectRatio,
				"the v1 task API has no aspect-ratio control; resolution tiers are fixed-ratio",
			)
		} else if !v2RatioValues[string(video.AspectRatio)] {
			ledger.reject(
				inference.FieldGenerateIntentVideoAspectRatio,
				fmt.Sprintf(
					"MiniMax-H3 ratios are adaptive/21:9/16:9/4:3/1:1/3:4/9:16, not %q",
					video.AspectRatio,
				),
			)
		} else {
			wire.ratio = string(video.AspectRatio)
		}
	} else if entry.videoV2 && v2TextOnly(&wire.v2Content) {
		// Text-only tasks require an explicit non-adaptive ratio; 16:9 is
		// the driver default.
		wire.ratio = "16:9"
	}
	if entry.videoV2 && wire.ratio == "adaptive" && v2TextOnly(&wire.v2Content) {
		ledger.reject(
			inference.FieldGenerateIntentVideoAspectRatio,
			"text-to-video requires an explicit ratio; adaptive is not allowed",
		)
	}
	if video.Seed != nil {
		ledger.reject(
			inference.FieldGenerateIntentVideoSeed,
			"the minimax task APIs have no seed control",
		)
	}
	wire.watermark = video.Watermark
}

// v2TextOnly reports whether the content carries no image or reference
// input, i.e. the request is a text-only (t2v) task.
func v2TextOnly(content *v2Content) bool {
	return content.firstFrame == "" && content.lastFrame == "" &&
		len(content.referenceImages) == 0 &&
		len(content.referenceVideos) == 0 && len(content.referenceAudios) == 0
}

// compileVideoOptions lowers VideoOptions onto the wire and rejects the
// v1-only prompt controls on the v2 API, where they do not exist.
func compileVideoOptions(
	wire *videoWire,
	options VideoOptions,
	entry catalogEntry,
	endpoint string,
	ledger *ledger,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	wire.callbackURL = options.CallbackURL
	if options.PromptOptimizer != nil {
		if entry.videoV2 {
			ledger.reject(
				field("prompt_optimizer"),
				fmt.Sprintf("%s does not support prompt_optimizer; the v2 API has no prompt optimizer", endpoint),
			)
		} else {
			wire.promptOptimizer = options.PromptOptimizer
		}
	}
	if options.FastPretreatment != nil {
		if entry.videoV2 {
			ledger.reject(
				field("fast_pretreatment"),
				fmt.Sprintf("%s does not support fast_pretreatment; the v2 API has no prompt optimizer", endpoint),
			)
		} else {
			wire.fastPretreatment = options.FastPretreatment
		}
	}
	if options.LastFrameOnly != nil && *options.LastFrameOnly {
		if !entry.videoV2 {
			ledger.reject(
				field("last_frame_only"),
				fmt.Sprintf("%s does not support last_frame_only; the v1 API has no last-frame-only task", endpoint),
			)
		} else if wire.firstFrame == "" || wire.lastFrame != "" {
			ledger.reject(
				field("last_frame_only"),
				"last_frame_only requires exactly one input image",
			)
		} else {
			// Mark the single image as the closing frame
			// (role=last_frame) instead of the opening one.
			wire.lastFrame = wire.firstFrame
			wire.firstFrame = ""
		}
	}
}

// videoCreateResponse is the v1 create-task envelope.
type videoCreateResponse struct {
	TaskID   string   `json:"task_id"`
	BaseResp baseResp `json:"base_resp"`
}

// videoV2CreateResponse is the v2 create-task envelope; v2 errors ride
// HTTP status codes rather than base_resp.
type videoV2CreateResponse struct {
	TaskID string `json:"task_id"`
}

// videoQueryResponse is the v1 task-status envelope.
type videoQueryResponse struct {
	TaskID   string   `json:"task_id"`
	Status   string   `json:"status"`
	FileID   string   `json:"file_id"`
	BaseResp baseResp `json:"base_resp"`
}

// videoV2QueryResponse is the shared v2 task-status envelope. Video tasks
// carry the artifact URL in content.url; H3-Context-IR tasks carry the
// enhanced prompt in content.prompt.
type videoV2QueryResponse struct {
	Task struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Content struct {
			URL    string `json:"url"`
			Prompt string `json:"prompt"`
		} `json:"content"`
		Usage struct {
			TotalTokens      int64 `json:"total_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	} `json:"task"`
}

// v2TaskUsage carries the token accounting a succeeded v2 task reports.
type v2TaskUsage struct {
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
}

// v2TaskResult is the succeeded v2 task content; exactly one of url and
// prompt is populated, depending on the task type.
type v2TaskResult struct {
	id     string
	url    string
	prompt string
	usage  v2TaskUsage
}

// pollV2Task polls the shared v2 query endpoint until the task reaches a
// terminal state. Video generation and H3-Context-IR both ride it; the
// caller validates the content field its task type requires.
func pollV2Task(
	ctx context.Context,
	client *mediaClient,
	taskID string,
	pollInterval time.Duration,
) (v2TaskResult, error) {
	queryPath := "/v2/query/video_generation/" + url.PathEscape(taskID)
	for {
		var task videoV2QueryResponse
		if err := client.getJSON(ctx, queryPath, nil, &task); err != nil {
			return v2TaskResult{}, err
		}
		switch strings.ToLower(task.Task.Status) {
		case "succeeded":
			return v2TaskResult{
				id:     task.Task.ID,
				url:    task.Task.Content.URL,
				prompt: task.Task.Content.Prompt,
				usage: v2TaskUsage{
					inputTokens:  task.Task.Usage.PromptTokens,
					outputTokens: task.Task.Usage.CompletionTokens,
					totalTokens:  task.Task.Usage.TotalTokens,
				},
			}, nil
		case "failed":
			return v2TaskResult{}, errdefs.NotAvailable(fmt.Errorf(
				"minimax: task %q failed server-side",
				taskID,
			))
		case "cancelled":
			return v2TaskResult{}, errdefs.NotAvailable(fmt.Errorf(
				"minimax: task %q was cancelled server-side",
				taskID,
			))
		}
		select {
		case <-ctx.Done():
			return v2TaskResult{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// videoFileResponse is the v1 file-retrieval envelope.
type videoFileResponse struct {
	File struct {
		DownloadURL string `json:"download_url"`
	} `json:"file"`
	BaseResp baseResp `json:"base_resp"`
}

func transportVideoV1(
	client *mediaClient,
	pollInterval time.Duration,
) inference.Transport[videoWire, videoRaw] {
	return func(ctx context.Context, wire videoWire) (videoRaw, error) {
		request := map[string]any{
			"model":  wire.model,
			"prompt": wire.prompt,
		}
		if wire.firstFrame != "" {
			request["first_frame_image"] = wire.firstFrame
		}
		if wire.lastFrame != "" {
			request["last_frame_image"] = wire.lastFrame
		}
		if wire.duration != nil {
			request["duration"] = *wire.duration
		}
		if wire.resolution != "" {
			request["resolution"] = wire.resolution
		}
		if wire.watermark != nil {
			request["aigc_watermark"] = *wire.watermark
		}
		if wire.promptOptimizer != nil {
			request["prompt_optimizer"] = *wire.promptOptimizer
		}
		if wire.fastPretreatment != nil {
			request["fast_pretreatment"] = *wire.fastPretreatment
		}
		if wire.callbackURL != "" {
			request["callback_url"] = wire.callbackURL
		}

		var created videoCreateResponse
		if err := client.postJSON(ctx, "/v1/video_generation", request, &created); err != nil {
			return videoRaw{}, err
		}
		if err := created.BaseResp.err("video generation"); err != nil {
			return videoRaw{}, err
		}
		if created.TaskID == "" {
			return videoRaw{}, fmt.Errorf(
				"minimax: video task creation returned no task_id",
			)
		}

		query := url.Values{"task_id": {created.TaskID}}
		// MiniMax's v1 video endpoint returns no request/trace id on
		// success; the task_id is the only server-assigned handle for the
		// create request, so it doubles as the request id for tracing.
		requestID := created.TaskID
		for {
			var task videoQueryResponse
			if err := client.getJSON(ctx, "/v1/query/video_generation", query, &task); err != nil {
				return videoRaw{}, err
			}
			if err := task.BaseResp.err("video task query"); err != nil {
				return videoRaw{}, err
			}
			switch strings.ToLower(task.Status) {
			case "success":
				if task.FileID == "" {
					return videoRaw{}, fmt.Errorf(
						"minimax: succeeded video task %q carries no file_id",
						created.TaskID,
					)
				}
				var file videoFileResponse
				fileQuery := url.Values{"file_id": {task.FileID}}
				if err := client.getJSON(ctx, "/v1/files/retrieve", fileQuery, &file); err != nil {
					return videoRaw{}, err
				}
				if err := file.BaseResp.err("video file retrieval"); err != nil {
					return videoRaw{}, err
				}
				if file.File.DownloadURL == "" {
					return videoRaw{}, fmt.Errorf(
						"minimax: video file %q carries no download url",
						task.FileID,
					)
				}
				return videoRaw{
					videoURL:  file.File.DownloadURL,
					requestID: requestID,
				}, nil
			case "fail", "failed":
				return videoRaw{}, errdefs.NotAvailable(fmt.Errorf(
					"minimax: video task %q failed server-side",
					created.TaskID,
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

func transportVideoV2(
	client *mediaClient,
	pollInterval time.Duration,
) inference.Transport[videoWire, videoRaw] {
	return func(ctx context.Context, wire videoWire) (videoRaw, error) {
		request := map[string]any{
			"model":      wire.model,
			"content":    v2ContentItems(wire.prompt, wire.v2Content),
			"resolution": wire.resolution,
		}
		if wire.duration != nil {
			request["duration"] = *wire.duration
		}
		if wire.ratio != "" {
			request["ratio"] = wire.ratio
		}
		if wire.watermark != nil {
			request["aigc_watermark"] = *wire.watermark
		}
		if wire.callbackURL != "" {
			request["callback_url"] = wire.callbackURL
		}

		var created videoV2CreateResponse
		if err := client.postJSON(ctx, "/v2/video_generation", request, &created); err != nil {
			return videoRaw{}, err
		}
		if created.TaskID == "" {
			return videoRaw{}, fmt.Errorf(
				"minimax: video task creation returned no task_id",
			)
		}
		task, err := pollV2Task(ctx, client, created.TaskID, pollInterval)
		if err != nil {
			return videoRaw{}, err
		}
		if task.url == "" {
			return videoRaw{}, fmt.Errorf(
				"minimax: succeeded video task %q carries no content url",
				created.TaskID,
			)
		}
		return videoRaw{
			videoURL:  task.url,
			requestID: task.id,
		}, nil
	}
}

// v2ContentItems renders the official v2 content array for a task: the
// mandatory text item first, then images and reference media with their
// roles.
func v2ContentItems(prompt string, content v2Content) []map[string]any {
	items := make([]map[string]any, 0,
		1+len(content.referenceImages)+len(content.referenceVideos)+len(content.referenceAudios))
	items = append(items, map[string]any{
		"type": "text",
		"text": prompt,
	})
	item := func(itemType, value, role string) map[string]any {
		return map[string]any{
			"type": itemType,
			itemType: map[string]any{
				"url": value,
			},
			"role": role,
		}
	}
	if content.firstFrame != "" {
		items = append(items, item("image_url", content.firstFrame, "first_frame"))
	}
	if content.lastFrame != "" {
		items = append(items, item("image_url", content.lastFrame, "last_frame"))
	}
	for _, value := range content.referenceImages {
		items = append(items, item("image_url", value, "reference_image"))
	}
	for _, value := range content.referenceVideos {
		items = append(items, item("video_url", value, "reference_video"))
	}
	for _, value := range content.referenceAudios {
		items = append(items, item("audio_url", value, "reference_audio"))
	}
	return items
}

func decodeVideo(
	_ context.Context,
	raw videoRaw,
) (inference.GenerateResponse, error) {
	// MiniMax video tasks deliver mp4 files; the URL carries no explicit
	// media type, so the compiler-known container is the truthful one.
	source, err := media.NewVideoURL(raw.videoURL, "video/mp4")
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("minimax: video url: %w", err)
	}
	generated := int64(1)
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{message.VideoPart{Source: source}}},
		},
		FinishReason: inference.FinishCompleted,
		Usage: inference.Usage{
			GeneratedVideos: &generated,
		},
		Metadata: inference.Metadata{RequestID: raw.requestID},
	}, nil
}

func openVideo(
	cls *clients,
	spec Spec,
	entry catalogEntry,
	id inference.ModelID,
) (inference.GenerateOperations, error) {
	var transport inference.Transport[videoWire, videoRaw]
	if entry.videoV2 {
		transport = transportVideoV2(cls.media, spec.videoPollInterval())
	} else {
		transport = transportVideoV1(cls.media, spec.videoPollInterval())
	}
	unary, err := inference.BindGenerate(
		compileVideo(wireModel(id.Name, entry), entry),
		transport,
		decodeVideo,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}

// videoImageValue renders an input image the way the video task APIs expect:
// absolute URLs pass through, inline bytes become a data URI carrying the
// source media type. (image_generation's subject_reference surface uses raw
// base64 instead — see imageSourceValue.)
func videoImageValue(source media.ImageSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}

func videoSourceURI(source media.VideoSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}

func audioSourceURI(source media.AudioSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes())
}
