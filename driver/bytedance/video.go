package bytedance

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/driver/internal/ptr"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

type videoRaw struct {
	videoURL         string
	completionTokens int64
}

const statusExpired = "expired"

func videoOptions(extensions inference.Extensions) (*VideoOptions, error) {
	var found *VideoOptions
	for _, extension := range extensions {
		options, ok := extension.(*VideoOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: extension %s/%s is not understood",
					extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: duplicate video options"))
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

func compileVideo(
	endpoint string,
	declared ModelSpec,
) protocol.Compiler[inference.GenerateRequest, *arkmodel.CreateContentGenerationTaskRequest] {
	return func(
		_ context.Context,
		request inference.GenerateRequest,
		shape protocol.Shape,
	) (protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest], error) {
		if request.Output.Kind() != inference.OutputVideo {
			return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldOutput,
				fmt.Errorf("bytedance: video pipeline serves video output only"))
		}
		if shape == protocol.ShapeStream {
			return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldStream,
				fmt.Errorf("bytedance: video generation is unary"))
		}
		options, err := videoOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, err
		}
		ark := &arkmodel.CreateContentGenerationTaskRequest{Model: endpoint}
		var prompt, images, videos, audios []string
		collect := func(parts []message.Part) error {
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
					return inference.NewError(inference.UnsupportedFeature, inference.FieldContent,
						fmt.Errorf("bytedance: video generation accepts text, image, video and audio parts, not %s", part.Kind()))
				}
			}
			return nil
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldContext,
					fmt.Errorf("bytedance: video generation keeps user context only"))
			}
			if err := collect(turn.Content.Parts); err != nil {
				return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, err
			}
		}
		if err := collect(request.Input.Content.Parts); err != nil {
			return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, err
		}
		joined := strings.Join(prompt, "\n")
		ark.Content = []*arkmodel.CreateContentGenerationContentItem{{
			Type: arkmodel.ContentGenerationContentItemTypeText, Text: &joined,
		}}
		referenceMode := len(images) > 2 || len(videos) > 0 || len(audios) > 0
		switch {
		case len(images) == 1:
			if referenceMode {
				return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("bytedance: first/last-frame and reference inputs are mutually exclusive"))
			}
			ark.Content = append(ark.Content, itemImage(images[0], "first_frame"))
		case len(images) == 2:
			if referenceMode {
				return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("bytedance: first/last-frame and reference inputs are mutually exclusive"))
			}
			ark.Content = append(ark.Content,
				itemImage(images[0], "first_frame"), itemImage(images[1], "last_frame"))
		case len(images) > 2:
			if declared.Video.ReferenceImage == 0 {
				return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("bytedance: model does not accept reference images"))
			}
			if len(images) > declared.Video.ReferenceImage {
				return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
					inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf("bytedance: model accepts at most %d reference images", declared.Video.ReferenceImage))
			}
			for _, url := range images {
				ark.Content = append(ark.Content, itemImage(url, "reference_image"))
			}
		}
		if len(videos) > 0 && (declared.Video.ReferenceVideo == 0 || len(videos) > declared.Video.ReferenceVideo) {
			return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("bytedance: model does not accept this many reference videos"))
		}
		for _, url := range videos {
			ark.Content = append(ark.Content, itemVideo(url, "reference_video"))
		}
		if len(audios) > 0 && (declared.Video.ReferenceAudio == 0 || len(audios) > declared.Video.ReferenceAudio) {
			return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("bytedance: model does not accept this many reference audio clips"))
		}
		for _, url := range audios {
			ark.Content = append(ark.Content, itemAudio(url, "reference_audio"))
		}
		if len(audios) > 0 && len(images) == 0 && len(videos) == 0 && !declared.Video.AudioOnly {
			return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("bytedance: model does not allow audio-only input"))
		}
		if options != nil {
			if err := applyVideoOptions(ark, options, declared); err != nil {
				return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{}, err
			}
		}
		return protocol.Compiled[*arkmodel.CreateContentGenerationTaskRequest]{
			Wire: ark, Fields: request.ActiveFields(),
		}, nil
	}
}

func applyVideoOptions(
	ark *arkmodel.CreateContentGenerationTaskRequest,
	options *VideoOptions,
	declared ModelSpec,
) error {
	if options.DurationMillis != nil {
		millis := *options.DurationMillis
		if millis%1000 != 0 {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: duration must be whole seconds"))
		}
		seconds := millis / 1000
		if min := declared.Video.DurationMin; min != nil && seconds < *min {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: duration must be at least %ds", *min))
		}
		if max := declared.Video.DurationMax; max != nil && seconds > *max {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: duration must be at most %ds", *max))
		}
		ark.Duration = &seconds
	}
	if options.Resolution != "" {
		resolution := strings.ToLower(options.Resolution)
		ark.Resolution = &resolution
		if cap := declared.MaxResolution; cap != "" && !resolutionWithin(resolution, cap) {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: resolution is capped at %s", cap))
		}
	}
	if options.AspectRatio != "" {
		ratio := string(options.AspectRatio)
		if !validVideoRatio(ratio) {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: unsupported ratio %q", ratio))
		}
		if declared.Video.FrameRatioAdaptiveOnly &&
			(hasContentRole(ark, "first_frame") || hasContentRole(ark, "last_frame")) &&
			ratio != "adaptive" {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: first/last-frame tasks require ratio=adaptive"))
		}
		ark.Ratio = &ratio
	}
	if options.Seed != nil {
		if !declared.Video.Seed {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: model does not support seed"))
		}
		ark.Seed = ptr.Clone(options.Seed)
	}
	if options.Watermark != nil {
		ark.Watermark = ptr.Clone(options.Watermark)
	}
	if options.CameraFixed != nil {
		if !declared.Video.CameraFixed {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: model does not support camera_fixed"))
		}
		ark.CameraFixed = ptr.Clone(options.CameraFixed)
	}
	if options.GenerateAudio != nil {
		if !declared.Video.GenerateAudio {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: model does not support generate_audio"))
		}
		ark.GenerateAudio = ptr.Clone(options.GenerateAudio)
	}
	if options.ServiceTier != "" {
		if options.ServiceTier == "flex" && !declared.Video.FlexTier {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: model does not support flex tier"))
		}
		tier := options.ServiceTier
		ark.ServiceTier = &tier
	}
	if options.Priority != nil {
		if !declared.Video.Priority {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: model does not support priority"))
		}
		ark.Priority = ptr.Clone(options.Priority)
	}
	if options.OutputFormat != nil {
		if !declared.Video.OutputFormat {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: model does not support output_format"))
		}
		format := *options.OutputFormat
		ark.OutputFormat = &format
	}
	if options.OmniReferenceTaskType != nil {
		if !declared.Video.OmniReference {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: model does not support omni_reference_task_type"))
		}
		taskType := *options.OmniReferenceTaskType
		ark.OmniReferenceTaskType = &taskType
	}
	if options.WebSearch != nil && *options.WebSearch {
		if !declared.Capabilities.HostedWebSearch {
			return inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("bytedance: model does not support web_search"))
		}
		ark.Tools = []*arkmodel.ContentGenerationTool{{Type: arkmodel.ToolTypeWebSearch}}
	}
	ark.ExecutionExpiresAfter = ptr.Clone(options.ExecutionExpiresAfter)
	if url := derefString(options.CallbackURL); url != "" {
		ark.CallbackUrl = &url
	}
	if id := derefString(options.SafetyIdentifier); id != "" {
		ark.SafetyIdentifier = &id
	}
	return nil
}

func hasContentRole(ark *arkmodel.CreateContentGenerationTaskRequest, role string) bool {
	for _, item := range ark.Content {
		if item.Role != nil && *item.Role == role {
			return true
		}
	}
	return false
}

func validVideoRatio(ratio string) bool {
	switch ratio {
	case "16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive":
		return true
	}
	return false
}

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
	res, ok := tier(resolution)
	if !ok {
		return false
	}
	limit, ok := tier(cap)
	return ok && res <= limit
}

func transportVideo(
	client *arkruntime.Client,
	pollInterval time.Duration,
	options []arkruntime.RequestOption,
) protocol.Transport[inference.GenerateRequest, *arkmodel.CreateContentGenerationTaskRequest, videoRaw] {
	return func(ctx context.Context, _ inference.GenerateRequest, request *arkmodel.CreateContentGenerationTaskRequest) (videoRaw, error) {
		created, err := client.CreateContentGenerationTask(ctx, *request, options...)
		if err != nil {
			return videoRaw{}, classifyError(err)
		}
		for {
			task, err := client.GetContentGenerationTask(ctx,
				arkmodel.GetContentGenerationTaskRequest{ID: created.ID}, options...)
			if err != nil {
				return videoRaw{}, classifyError(err)
			}
			switch task.Status {
			case arkmodel.StatusSucceeded:
				if task.Content.VideoURL == "" {
					return videoRaw{}, fmt.Errorf("bytedance: succeeded video task %q carries no video url", task.ID)
				}
				return videoRaw{videoURL: task.Content.VideoURL, completionTokens: int64(task.Usage.CompletionTokens)}, nil
			case arkmodel.StatusFailed:
				code, message := "", ""
				if task.Error != nil {
					code, message = task.Error.Code, task.Error.Message
				}
				return videoRaw{}, classifyResponseError(code, message)
			case arkmodel.StatusCancelled:
				return videoRaw{}, errdefs.NotAvailable(fmt.Errorf("bytedance: video task %q was cancelled", task.ID))
			case statusExpired:
				return videoRaw{}, errdefs.Timeout(fmt.Errorf("bytedance: video task %q expired", task.ID))
			}
			select {
			case <-ctx.Done():
				return videoRaw{}, ctx.Err()
			case <-time.After(pollInterval):
			}
		}
	}
}

func itemImage(url, role string) *arkmodel.CreateContentGenerationContentItem {
	return &arkmodel.CreateContentGenerationContentItem{
		Type:     arkmodel.ContentGenerationContentItemTypeImage,
		ImageURL: &arkmodel.ImageURL{URL: url}, Role: &role,
	}
}

func itemVideo(url, role string) *arkmodel.CreateContentGenerationContentItem {
	return &arkmodel.CreateContentGenerationContentItem{
		Type:     arkmodel.ContentGenerationContentItemTypeVideo,
		VideoURL: &arkmodel.VideoUrl{Url: url}, Role: &role,
	}
}

func itemAudio(url, role string) *arkmodel.CreateContentGenerationContentItem {
	return &arkmodel.CreateContentGenerationContentItem{
		Type:     arkmodel.ContentGenerationContentItemTypeAudio,
		AudioURL: &arkmodel.AudioUrl{Url: url}, Role: &role,
	}
}

func decodeVideo(
	_ context.Context,
	request inference.GenerateRequest,
	raw videoRaw,
) (inference.GenerateResponse, error) {
	source, err := media.NewVideoURL(raw.videoURL, "video/mp4")
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("bytedance: video url: %w", err)
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{message.VideoPart{Source: source}}},
		},
		FinishReason: inference.FinishStop,
		Usage: usage.Usage{
			OutputTokens: raw.completionTokens,
			Work:         map[usage.WorkKind]int64{usage.WorkVideosGenerated: 1},
		},
		Metadata: inference.Metadata{Model: request.Model.ID},
	}, nil
}
