package minimax

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// Video generation runs on MiniMax's async task pipeline: create the task,
// poll its status, then retrieve the finished file's download URL. The
// download URL expires one hour after retrieval, so the response carries it
// verbatim — downloading and persisting is the caller's job. Durations are
// model-bound (6s everywhere, 10s at 768P on the Hailuo-2.3/02 pair), and
// the endpoint has no aspect-ratio or seed knob, so those intents reject.
// Hailuo-2.3-Fast is image-to-video only: without a first-frame image the
// compile rejects rather than letting the API invent one.

type videoWire struct {
	model      string
	prompt     string
	firstFrame string
	duration   int // seconds: 6 or 10
	resolution string
	watermark  *bool
}

type videoRaw struct {
	videoURL string
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

		var prompt []string
		collect := func(parts []message.Part, fields map[message.PartKind]inference.FieldID) {
			for _, part := range parts {
				switch value := part.(type) {
				case message.TextPart:
					prompt = append(prompt, value.Text)
				case message.ImagePart:
					if wire.firstFrame != "" {
						ledger.reject(
							fields[part.Kind()],
							"video generation accepts a single first-frame image",
						)
						continue
					}
					wire.firstFrame = imageSourceValue(value.Source)
				default:
					ledger.reject(
						fields[part.Kind()],
						fmt.Sprintf("video generation accepts text and image parts, not %s", part.Kind()),
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
			if video.DurationMillis != nil {
				millis := *video.DurationMillis
				switch {
				case millis%1000 != 0:
					ledger.reject(
						inference.FieldGenerateIntentVideoDuration,
						fmt.Sprintf("minimax video durations are whole seconds, not %dms", millis),
					)
				default:
					seconds := int(millis / 1000)
					switch {
					case seconds == 6:
						wire.duration = 6
					case seconds == 10 && !entry.video10s:
						ledger.reject(
							inference.FieldGenerateIntentVideoDuration,
							fmt.Sprintf("%s serves 6-second videos only", endpoint),
						)
					case seconds == 10:
						wire.duration = 10
					default:
						ledger.reject(
							inference.FieldGenerateIntentVideoDuration,
							fmt.Sprintf("minimax video durations are 6s or 10s, not %ds", seconds),
						)
					}
				}
			}
			if video.Resolution != "" {
				switch strings.ToUpper(video.Resolution) {
				case "768P":
					wire.resolution = "768P"
				case "1080P":
					if !entry.videoHD {
						ledger.reject(
							inference.FieldGenerateIntentVideoResolution,
							fmt.Sprintf("%s serves 768P only", endpoint),
						)
					} else {
						wire.resolution = "1080P"
					}
				default:
					ledger.reject(
						inference.FieldGenerateIntentVideoResolution,
						fmt.Sprintf("%s serves 768P/1080P tiers, not %q", endpoint, video.Resolution),
					)
				}
			}
			if wire.duration == 10 && wire.resolution == "1080P" {
				ledger.reject(
					inference.FieldGenerateIntentVideoDuration,
					"10-second videos require 768P",
				)
			}
			if video.AspectRatio != "" {
				ledger.reject(
					inference.FieldGenerateIntentVideoAspectRatio,
					"the task API has no aspect-ratio control; resolution tiers are fixed-ratio",
				)
			}
			if video.Seed != nil {
				ledger.reject(
					inference.FieldGenerateIntentVideoSeed,
					"the task API has no seed control",
				)
			}
			wire.watermark = video.Watermark
		}
		rejectOtherExtensions("video generation", request.Extensions, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[videoWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[videoWire]{Wire: wire, Report: report}, nil
	}
}

// videoCreateResponse is the create-task envelope.
type videoCreateResponse struct {
	TaskID   string   `json:"task_id"`
	BaseResp baseResp `json:"base_resp"`
}

// videoQueryResponse is the task-status envelope.
type videoQueryResponse struct {
	TaskID   string   `json:"task_id"`
	Status   string   `json:"status"`
	FileID   string   `json:"file_id"`
	BaseResp baseResp `json:"base_resp"`
}

// videoFileResponse is the file-retrieval envelope.
type videoFileResponse struct {
	File struct {
		DownloadURL string `json:"download_url"`
	} `json:"file"`
	BaseResp baseResp `json:"base_resp"`
}

func transportVideo(
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
		if wire.duration != 0 {
			request["duration"] = wire.duration
		}
		if wire.resolution != "" {
			request["resolution"] = wire.resolution
		}
		if wire.watermark != nil {
			request["aigc_watermark"] = *wire.watermark
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
				return videoRaw{videoURL: file.File.DownloadURL}, nil
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

func decodeVideo(
	_ context.Context,
	raw videoRaw,
) (inference.GenerateResponse, error) {
	// Hailuo tasks deliver mp4 files; the URL carries no explicit media
	// type, so the compiler-known container is the truthful one.
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
	}, nil
}

func openVideo(
	cls *clients,
	spec Spec,
	entry catalogEntry,
	id inference.ModelID,
) (inference.GenerateOperations, error) {
	unary, err := inference.BindGenerate(
		compileVideo(id.Name, entry),
		transportVideo(cls.media, spec.videoPollInterval()),
		decodeVideo,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
