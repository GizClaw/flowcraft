package bytedance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"

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
// Input images map onto the frame roles the task API understands: the first
// image is the first frame, the second is the last frame. More references
// have no truthful canonical ordering, so the compiler rejects them instead
// of guessing. Video-reference input (video_url content items) is not
// exposed: the pinned SDK version cannot encode it.

type videoWire struct {
	model      string
	prompt     string
	firstFrame string
	lastFrame  string
	duration   *int64 // seconds
	resolution string // 480p | 720p | 1080p | 4k
	ratio      string // width:height, e.g. 16:9
	seed       *int64
	watermark  *bool
	// Extension settings (VideoOptions).
	cameraFixed           *bool
	generateAudio         *bool
	serviceTier           string
	executionExpiresAfter *int64
}

type videoRaw struct {
	videoURL         string
	completionTokens int64
}

// defaultVideoPollInterval paces task polls when the deployment Spec does
// not override it (Spec.video_poll_interval_millis).
const defaultVideoPollInterval = 5 * time.Second

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
		collect := func(parts []inference.Part, fields map[inference.PartKind]inference.FieldID) {
			for _, part := range parts {
				switch value := part.(type) {
				case inference.TextPart:
					prompt = append(prompt, value.Text)
				case inference.ImagePart:
					images = append(images, sourceURI(value.Source))
				case inference.VideoPart:
					ledger.reject(
						fields[part.Kind()],
						"video-reference input is not exposed by the pinned SDK",
					)
				default:
					ledger.reject(
						fields[part.Kind()],
						fmt.Sprintf("video generation accepts text and image parts, not %s", part.Kind()),
					)
				}
			}
		}
		for _, message := range request.Context {
			if message.Role != inference.RoleUser {
				ledger.reject(
					inference.FieldGenerateContextRole,
					"video generation keeps user context only; assistant, system, and tool turns have no native channel",
				)
				continue
			}
			collect(message.Content.Parts, contextPartFields)
		}
		collect(request.Input.Content.Parts, inputPartFields)
		wire.prompt = strings.Join(prompt, "\n")
		switch len(images) {
		case 0:
		case 1:
			wire.firstFrame = images[0]
		case 2:
			wire.firstFrame, wire.lastFrame = images[0], images[1]
		default:
			ledger.reject(
				inference.FieldGenerateInputImage,
				"video generation accepts at most a first-frame and a last-frame image",
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
			}
			wire.seed = video.Seed
			wire.watermark = video.Watermark
		}
		options, other := operationExtensions[VideoOptions](request.Extensions)
		rejectOtherExtensions("video generation", other, ledger)
		compileVideoOptions(&wire, options)

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

// compileVideoOptions lowers VideoOptions onto the wire. None of the
// extension fields overlap a canonical channel, so every value applies
// directly.
func compileVideoOptions(wire *videoWire, options VideoOptions) {
	wire.cameraFixed = options.CameraFixed
	wire.generateAudio = options.GenerateAudio
	wire.serviceTier = options.ServiceTier
	wire.executionExpiresAfter = options.ExecutionExpiresAfter
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
		frame := func(url, role string) *arkmodel.CreateContentGenerationContentItem {
			return &arkmodel.CreateContentGenerationContentItem{
				Type:     arkmodel.ContentGenerationContentItemTypeImage,
				ImageURL: &arkmodel.ImageURL{URL: url},
				Role:     &role,
			}
		}
		if wire.firstFrame != "" {
			content = append(content, frame(wire.firstFrame, "first_frame"))
		}
		if wire.lastFrame != "" {
			content = append(content, frame(wire.lastFrame, "last_frame"))
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
			}
			select {
			case <-ctx.Done():
				return videoRaw{}, ctx.Err()
			case <-time.After(pollInterval):
			}
		}
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
		Message: inference.Message{
			Role:    inference.RoleAssistant,
			Content: inference.Content{Parts: []inference.Part{inference.VideoPart{Source: source}}},
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
	if err := cls.requireArkAPIKey(profile, "Seedance video generation"); err != nil {
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
