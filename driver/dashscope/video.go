package dashscope

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

const videoPathSynthesis = "/api/v1/services/aigc/video-generation/video-synthesis"

type videoWire struct {
	model         string
	prompt        string
	image         string
	resolution    string
	ratio         string
	duration      *int
	seed          *int64
	watermark     *bool
	negative      string
	promptExtend  *bool
	shotType      string
	audio         *bool
	template      string
	link          string
	smartDuration bool
	media         []mediaEntry
	mediaMode     bool
}

type mediaEntry struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

func videoOptions(extensions inference.Extensions) (*VideoOptions, error) {
	var found *VideoOptions
	for _, extension := range extensions {
		options, ok := extension.(*VideoOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: extension %s/%s is not understood", extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: duplicate video options"))
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

func compileVideo(declared ModelSpec, name string) protocol.Compiler[inference.GenerateRequest, videoWire] {
	return func(_ context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[videoWire], error) {
		if request.Output.Kind() != inference.OutputVideo {
			return protocol.Compiled[videoWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldOutput, fmt.Errorf("dashscope: video pipeline serves video output"))
		}
		if shape == protocol.ShapeStream {
			return protocol.Compiled[videoWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldStream, fmt.Errorf("dashscope: video generation is unary"))
		}
		options, err := videoOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[videoWire]{}, err
		}
		wire := videoWire{model: wireModel(name, declared), mediaMode: declared.Video.Media != nil}
		var prompts []string
		images := 0
		collect := func(parts []message.Part) error {
			for _, part := range parts {
				switch value := part.(type) {
				case message.TextPart:
					prompts = append(prompts, value.Text)
				case message.ImagePart:
					if !slicesContains(declared.Capabilities.Inputs, message.PartImage) {
						return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept image input"))
					}
					uri, err := imageSourceValue(value.Source)
					if err != nil {
						return err
					}
					if !wire.mediaMode {
						wire.image = uri
						continue
					}
					images++
					switch {
					case declared.Video.Media.FirstFrame && images == 1:
						wire.media = append(wire.media, mediaEntry{Type: "first_frame", URL: uri})
					case declared.Video.Media.LastFrame && images == 2:
						wire.media = append(wire.media, mediaEntry{Type: "last_frame", URL: uri})
					case declared.Video.Media.ReferenceImages > 0:
						if countMedia(wire.media, "reference_image") >= declared.Video.Media.ReferenceImages {
							return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: too many reference images"))
						}
						wire.media = append(wire.media, mediaEntry{Type: "reference_image", URL: uri})
					default:
						return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model has no home for another image"))
					}
				case message.VideoPart:
					if !wire.mediaMode || declared.Video.Media.ReferenceVideos == 0 {
						return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept reference video"))
					}
					wire.media = append(wire.media, mediaEntry{Type: "reference_video", URL: value.Source.URL()})
				case message.AudioPart:
					if !wire.mediaMode || declared.Video.Media.ReferenceAudios == 0 {
						return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept reference audio"))
					}
					wire.media = append(wire.media, mediaEntry{Type: "reference_audio", URL: value.Source.URL()})
				case message.FilePart:
					if !wire.mediaMode || !declared.Video.Media.File {
						return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: model does not accept file input"))
					}
					if value.Source.Kind() != media.SourceURL {
						return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: file input must be a URL"))
					}
					wire.media = append(wire.media, mediaEntry{Type: "file", URL: value.Source.URL()})
				default:
					return inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: video generation accepts text, image, video, audio and file parts, not %s", part.Kind()))
				}
			}
			return nil
		}
		for _, turn := range request.Context {
			if turn.Role != message.RoleUser {
				return protocol.Compiled[videoWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContext, fmt.Errorf("dashscope: video generation keeps user context only"))
			}
			if err := collect(turn.Content.Parts); err != nil {
				return protocol.Compiled[videoWire]{}, err
			}
		}
		if err := collect(request.Input.Content.Parts); err != nil {
			return protocol.Compiled[videoWire]{}, err
		}
		wire.prompt = strings.Join(prompts, "\n")
		if options != nil {
			if options.DurationMillis != nil {
				seconds := int(*options.DurationMillis / 1000)
				wire.duration = &seconds
			}
			wire.resolution = options.Resolution
			wire.ratio = string(options.AspectRatio)
			wire.seed = options.Seed
			wire.watermark = options.Watermark
			wire.negative = options.NegativePrompt
			wire.promptExtend = options.PromptExtend
			wire.shotType = options.ShotType
			wire.audio = options.Audio
			wire.template = options.Template
			wire.link = options.Link
			wire.smartDuration = options.SmartDuration
		}
		if wire.mediaMode {
			if wire.smartDuration && wire.duration != nil {
				return protocol.Compiled[videoWire]{}, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: smart_duration conflicts with an explicit duration"))
			}
			if wire.link != "" {
				if !declared.Video.Media.Link {
					return protocol.Compiled[videoWire]{}, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: model does not accept a link entry"))
				}
				wire.media = append(wire.media, mediaEntry{Type: "link", URL: wire.link})
			}
			if wire.smartDuration {
				wire.duration = intPtr(-1)
			}
		}
		return protocol.Compiled[videoWire]{Wire: wire, Fields: request.ActiveFields()}, nil
	}
}

func countMedia(entries []mediaEntry, kind string) int {
	n := 0
	for _, entry := range entries {
		if entry.Type == kind {
			n++
		}
	}
	return n
}
func intPtr(v int) *int { return &v }

// videoTaskBody is the create-task request for the synthesis path. The media array
// and the single-image field are alternatives: whichever the compiler set is what
// goes out, and the rest disappear.
type videoTaskBody struct {
	Model      string              `json:"model"`
	Input      videoTaskInput      `json:"input"`
	Parameters videoTaskParameters `json:"parameters"`
}

type videoTaskInput struct {
	Prompt string       `json:"prompt"`
	ImgURL string       `json:"img_url,omitempty"`
	Media  []mediaEntry `json:"media,omitempty"`
}

type videoTaskParameters struct {
	Resolution     string `json:"resolution,omitempty"`
	Ratio          string `json:"ratio,omitempty"`
	Duration       *int   `json:"duration,omitempty"`
	Seed           *int64 `json:"seed,omitempty"`
	Watermark      *bool  `json:"watermark,omitempty"`
	NegativePrompt string `json:"negative_prompt,omitempty"`
	PromptExtend   *bool  `json:"prompt_extend,omitempty"`
	ShotType       string `json:"shot_type,omitempty"`
	Audio          *bool  `json:"audio,omitempty"`
	Template       string `json:"template,omitempty"`
}

func videoTaskRequest(wire videoWire) videoTaskBody {
	input := videoTaskInput{Prompt: wire.prompt}
	if wire.mediaMode {
		if len(wire.media) > 0 {
			input.Media = wire.media
		}
	} else if wire.image != "" {
		input.ImgURL = wire.image
	}
	return videoTaskBody{
		Model: wire.model,
		Input: input,
		Parameters: videoTaskParameters{
			Resolution:     wire.resolution,
			Ratio:          wire.ratio,
			Duration:       wire.duration,
			Seed:           wire.seed,
			Watermark:      wire.watermark,
			NegativePrompt: wire.negative,
			PromptExtend:   wire.promptExtend,
			ShotType:       wire.shotType,
			Audio:          wire.audio,
			Template:       wire.template,
		},
	}
}

func transportVideo(client *mediaClient) protocol.Transport[inference.GenerateRequest, videoWire, videoRaw] {
	return func(ctx context.Context, _ inference.GenerateRequest, wire videoWire) (videoRaw, error) {
		taskID, requestID, err := createTask(ctx, client, videoPathSynthesis, videoTaskRequest(wire), "video generation")
		if err != nil {
			return videoRaw{}, err
		}
		query, err := awaitTask(ctx, client, client.pollInterval(), taskID, "video generation")
		if err != nil {
			return videoRaw{}, err
		}
		if query.Output.VideoURL == "" {
			return videoRaw{}, fmt.Errorf("dashscope: video generation returned no video_url")
		}
		return videoRaw{url: query.Output.VideoURL, requestID: nonEmpty(query.RequestID, requestID)}, nil
	}
}

type videoRaw struct {
	url       string
	requestID string
}

func decodeVideo(_ context.Context, request inference.GenerateRequest, raw videoRaw) (inference.GenerateResponse, error) {
	source, err := media.NewVideoURL(raw.url, "video/mp4")
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	return inference.GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{message.VideoPart{Source: source}}}},
		FinishReason: inference.FinishStop, Usage: usage.Usage{Work: map[usage.WorkKind]int64{usage.WorkVideosGenerated: 1}},
		Metadata: inference.Metadata{Model: request.Model.ID, RequestID: raw.requestID}}, nil
}

func (d *driver) compileVideo(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[videoWire], error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[videoWire]{}, err
	}
	return compileVideo(declared, request.Model.ID.Name)(ctx, request, shape)
}

func slicesContains[T comparable](items []T, want T) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
