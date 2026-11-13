package minimax

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Video generation runs on MiniMax's v2 asynchronous task pipeline: create the
// task, poll its status, then surface the finished artifact's URL. The download
// URL expires one hour after retrieval, so the response carries it verbatim —
// downloading and persisting is the caller's job.
//
// The v2 API serves MiniMax-H3 with a multimodal content array (text/image/
// video/audio items carrying first_frame/last_frame/reference_* roles), a
// required resolution (768P/2K) and duration (4-15s), an optional ratio
// (text-only tasks default to 16:9 and reject adaptive; image tasks default to
// adaptive server-side), and aigc_watermark/callback_url. Its query endpoint
// returns the artifact URL directly, so there is no file-retrieval hop. The
// flat v1 API the Hailuo 2.x/02 trio rides is not served here: those models are
// not declared by this driver, so a Hailuo declaration fails closed at the
// endpoint rather than compiling onto an API its model does not speak.
//
// Inline media become data URIs carrying their source media type; absolute URLs
// pass through verbatim.

const (
	// videoPathCreate is the v2 create-task endpoint.
	videoPathCreate = "/v2/video_generation"
	// videoPathQueryPrefix is the v2 task-status endpoint; the task id is
	// appended path-escaped.
	videoPathQueryPrefix = "/v2/query/video_generation/"

	// videoPromptLimit caps the prompt in characters, per the platform's
	// documented H3 limit.
	videoPromptLimit = 7000
	// referenceImageMax, referenceVideoMax and referenceAudioMax bound the
	// multimodal reference inputs the v2 content array accepts.
	referenceImageMax = 9
	referenceVideoMax = 3
	referenceAudioMax = 3
)

// The v2 duration envelope, in whole seconds.
const (
	videoDurationMin       = 4
	videoDurationMax       = 15
	videoDurationDefault   = 6
	videoResolutionDefault = "768P"
	// videoRatioTextDefault is the ratio a text-only task gets when the
	// request names none: the v2 schema requires an explicit non-adaptive
	// ratio for text-to-video.
	videoRatioTextDefault = "16:9"
)

// videoRatios are the official v2 ratio values. adaptive is the server default
// for image and reference tasks and invalid for text-only tasks.
var videoRatios = map[string]bool{
	"adaptive": true, "21:9": true, "16:9": true, "4:3": true,
	"1:1": true, "3:4": true, "9:16": true,
}

// videoResolutions are the official v2 resolution tiers.
var videoResolutions = map[string]bool{"768P": true, "2K": true}

// resolutionList names the served tiers for an error message.
func resolutionList() string {
	names := make([]string, 0, len(videoResolutions))
	for name := range videoResolutions {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, "/")
}

// videoWire is one compiled H3 task request.
type videoWire struct {
	model      string
	prompt     string
	duration   *int // seconds; nil only if a caller bypasses the compiler
	resolution string
	ratio      string
	watermark  *bool
	callback   string

	firstFrame      string
	lastFrame       string
	referenceImages []string
	referenceVideos []string
	referenceAudios []string
}

// videoRaw is one finished task: the artifact URL plus the accounting the query
// reported.
type videoRaw struct {
	videoURL string
	// requestID is the server-assigned task id. The v2 create response carries
	// no trace id, so the task id is the only handle the call has.
	requestID    string
	outputTokens int64
}

// textOnly reports whether the content carries no image or reference input,
// i.e. the task is text-to-video.
func (w videoWire) textOnly() bool {
	return w.firstFrame == "" && w.lastFrame == "" &&
		len(w.referenceImages) == 0 &&
		len(w.referenceVideos) == 0 && len(w.referenceAudios) == 0
}

func compileVideo(
	declared ModelSpec,
	name string,
) protocol.Compiler[inference.GenerateRequest, videoWire] {
	return func(
		_ context.Context,
		request inference.GenerateRequest,
		shape protocol.Shape,
	) (protocol.Compiled[videoWire], error) {
		if request.Output.Kind() != inference.OutputVideo {
			return protocol.Compiled[videoWire]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldOutput,
				fmt.Errorf("minimax: video pipeline serves video output only"))
		}
		if shape == protocol.ShapeStream {
			return protocol.Compiled[videoWire]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldStream,
				fmt.Errorf("minimax: video generation is a unary task on this provider"))
		}
		options, err := videoOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[videoWire]{}, err
		}
		wire := videoWire{model: wireModel(name, declared)}
		limits := declared.videoLimits()

		var prompt []string
		images, videos, audios, err := collectTaskParts(request, declared, &prompt)
		if err != nil {
			return protocol.Compiled[videoWire]{}, err
		}
		wire.prompt = strings.Join(prompt, "\n")
		if strings.TrimSpace(wire.prompt) == "" {
			return protocol.Compiled[videoWire]{}, inference.NewError(
				inference.UnsupportedFeature, inference.FieldContent,
				fmt.Errorf("minimax: %s requires a non-empty text prompt", name))
		}
		characters := len([]rune(wire.prompt))
		if characters > limits.promptLimit {
			return protocol.Compiled[videoWire]{}, inference.NewError(
				inference.InvalidRequest, inference.FieldContent,
				fmt.Errorf("minimax: prompts are at most %d characters, not %d",
					limits.promptLimit, characters))
		}
		if err := compileVideoInputs(&wire, images, videos, audios, name, limits); err != nil {
			return protocol.Compiled[videoWire]{}, err
		}
		if err := compileVideoOptions(&wire, options, name, limits); err != nil {
			return protocol.Compiled[videoWire]{}, err
		}
		return protocol.Compiled[videoWire]{
			Wire: wire, Fields: request.ActiveFields(),
		}, nil
	}
}

// collectTaskParts gathers text/image/video/audio parts from the user context
// turns and the request input, rendering media the way the task API expects:
// absolute URLs pass through, inline bytes become data URIs carrying their
// source media type.
//
// A non-empty capabilities.inputs list is the declaration's promise about what
// this model accepts, and nothing upstream enforces it, so the compiler is the
// gate: a part kind the model does not declare is rejected here rather than
// turned into a differently-priced operation on the wire. An empty list filters
// nothing, which is the documented meaning of an undeclared input surface.
func collectTaskParts(
	request inference.GenerateRequest,
	declared ModelSpec,
	prompt *[]string,
) (images, videos, audios []string, err error) {
	declares := func(kind message.PartKind) bool {
		return len(declared.Capabilities.Inputs) == 0 ||
			slices.Contains(declared.Capabilities.Inputs, kind)
	}
	collect := func(parts []message.Part) error {
		for _, part := range parts {
			switch value := part.(type) {
			case message.TextPart:
				*prompt = append(*prompt, value.Text)
			case message.ImagePart:
				if !declares(message.PartImage) {
					return undeclaredInput(declared, message.PartImage)
				}
				uri, err := imageSourceURI(value.Source, declared)
				if err != nil {
					return err
				}
				images = append(images, uri)
			case message.VideoPart:
				if !declares(message.PartVideo) {
					return undeclaredInput(declared, message.PartVideo)
				}
				uri, err := videoSourceURI(value.Source, declared)
				if err != nil {
					return err
				}
				videos = append(videos, uri)
			case message.AudioPart:
				if !declares(message.PartAudio) {
					return undeclaredInput(declared, message.PartAudio)
				}
				uri, err := audioSourceURI(value.Source, declared)
				if err != nil {
					return err
				}
				audios = append(audios, uri)
			default:
				return inference.NewError(
					inference.UnsupportedFeature, inference.FieldContent,
					fmt.Errorf(
						"minimax: the task accepts text, image, video, and audio parts, not %s",
						part.Kind()))
			}
		}
		return nil
	}
	for _, turn := range request.Context {
		if turn.Role != message.RoleUser {
			return nil, nil, nil, inference.NewError(
				inference.UnsupportedFeature, inference.FieldContext,
				fmt.Errorf("minimax: task generation keeps user context only"))
		}
		if err := collect(turn.Content.Parts); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := collect(request.Input.Content.Parts); err != nil {
		return nil, nil, nil, err
	}
	return images, videos, audios, nil
}

// undeclaredInput rejects a part kind the model's declaration does not list.
func undeclaredInput(declared ModelSpec, kind message.PartKind) error {
	return inference.NewError(
		inference.UnsupportedFeature, inference.FieldContent,
		fmt.Errorf("minimax: %s does not declare %s input", declared.Name, kind))
}

// videoLimits is the effective envelope for one video model: a declared leaf wins,
// and an omitted one keeps the platform default the driver documents.
type videoLimits struct {
	promptLimit     int
	durationMin     int
	durationMax     int
	resolutions     map[string]bool
	ratios          map[string]bool
	referenceImages int
	referenceVideos int
	referenceAudios int
}

// videoLimits resolves the declaration onto the platform envelope.
func (m ModelSpec) videoLimits() videoLimits {
	limits := videoLimits{
		promptLimit:     videoPromptLimit,
		durationMin:     videoDurationMin,
		durationMax:     videoDurationMax,
		resolutions:     videoResolutions,
		ratios:          videoRatios,
		referenceImages: referenceImageMax,
		referenceVideos: referenceVideoMax,
		referenceAudios: referenceAudioMax,
	}
	if m.Video.PromptLimit > 0 {
		limits.promptLimit = m.Video.PromptLimit
	}
	if m.Video.DurationMin != nil {
		limits.durationMin = *m.Video.DurationMin
	}
	if m.Video.DurationMax != nil {
		limits.durationMax = *m.Video.DurationMax
	}
	if len(m.Video.Resolutions) > 0 {
		limits.resolutions = nameSet(m.Video.Resolutions)
	}
	if len(m.Video.Ratios) > 0 {
		limits.ratios = nameSet(m.Video.Ratios)
	}
	if m.Video.ReferenceImages > 0 {
		limits.referenceImages = m.Video.ReferenceImages
	}
	if m.Video.ReferenceVideos > 0 {
		limits.referenceVideos = m.Video.ReferenceVideos
	}
	if m.Video.ReferenceAudios > 0 {
		limits.referenceAudios = m.Video.ReferenceAudios
	}
	return limits
}

// nameSet indexes a declared list for membership checks.
func nameSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

// compileVideoInputs maps media parts onto the v2 content roles. One image is
// the frame, two are first/last frames, and more than two — or any
// video/audio part — switch the task to multimodal reference generation; the
// two scenarios are mutually exclusive, mirroring the official content rules.
func compileVideoInputs(
	wire *videoWire,
	images, videos, audios []string,
	name string,
	limits videoLimits,
) error {
	firstLast := len(images) == 1 || len(images) == 2
	reference := len(images) > 2 || len(videos) > 0 || len(audios) > 0
	if firstLast && reference {
		return inference.NewError(
			inference.UnsupportedFeature, inference.FieldContent,
			fmt.Errorf("minimax: %s treats first/last-frame input and reference inputs as mutually exclusive", name))
	}
	switch {
	case len(images) == 1:
		wire.firstFrame = images[0]
	case len(images) == 2:
		wire.firstFrame, wire.lastFrame = images[0], images[1]
	case len(images) > limits.referenceImages:
		return inference.NewError(
			inference.UnsupportedFeature, inference.FieldContent,
			fmt.Errorf("minimax: %s accepts at most %d reference images", name, limits.referenceImages))
	case len(images) > 2:
		wire.referenceImages = images
	}
	if len(videos) > limits.referenceVideos {
		return inference.NewError(
			inference.UnsupportedFeature, inference.FieldContent,
			fmt.Errorf("minimax: %s accepts at most %d reference videos", name, limits.referenceVideos))
	}
	wire.referenceVideos = videos
	if len(audios) > limits.referenceAudios {
		return inference.NewError(
			inference.UnsupportedFeature, inference.FieldContent,
			fmt.Errorf("minimax: %s accepts at most %d reference audio clips", name, limits.referenceAudios))
	}
	wire.referenceAudios = audios
	return nil
}

// compileVideoOptions lowers VideoOptions onto the wire and enforces the declared
// envelope for this model.
func compileVideoOptions(
	wire *videoWire,
	options *VideoOptions,
	name string,
	limits videoLimits,
) error {
	field := func(option string) inference.Field {
		if options == nil {
			return inference.FieldExtensions
		}
		return inference.ExtensionField(option).Qualify(*options)
	}
	if options != nil {
		if options.LastFrameOnly != nil && *options.LastFrameOnly {
			if wire.firstFrame == "" || wire.lastFrame != "" {
				return inference.NewError(
					inference.InvalidExtension, field("last_frame_only"),
					fmt.Errorf("minimax: last_frame_only requires exactly one input image"))
			}
			// Mark the single image as the closing frame (role=last_frame)
			// instead of the opening one.
			wire.lastFrame = wire.firstFrame
			wire.firstFrame = ""
		}
		wire.watermark = options.Watermark
		wire.callback = options.CallbackURL
	}

	seconds := videoDurationDefault
	if options != nil && options.DurationMillis != nil {
		millis := *options.DurationMillis
		if millis%1000 != 0 {
			return inference.NewError(
				inference.InvalidExtension, field("duration_millis"),
				fmt.Errorf("minimax: durations are whole seconds, not %dms", millis))
		}
		seconds = int(millis / 1000)
	}
	if seconds < limits.durationMin || seconds > limits.durationMax {
		return inference.NewError(
			inference.InvalidExtension, field("duration_millis"),
			fmt.Errorf("minimax: %s durations are %d-%ds, not %ds",
				name, limits.durationMin, limits.durationMax, seconds))
	}
	wire.duration = &seconds

	wire.resolution = videoResolutionDefault
	if options != nil && options.Resolution != "" {
		if !limits.resolutions[options.Resolution] {
			return inference.NewError(
				inference.InvalidExtension, field("resolution"),
				fmt.Errorf("minimax: %s does not serve the %q tier",
					name, options.Resolution))
		}
		wire.resolution = options.Resolution
	}

	textOnly := wire.textOnly()
	if options != nil && options.AspectRatio != "" {
		ratio := string(options.AspectRatio)
		if !limits.ratios[ratio] {
			return inference.NewError(
				inference.InvalidExtension, field("aspect_ratio"),
				fmt.Errorf("minimax: %s does not serve the %q ratio", name, ratio))
		}
		if textOnly && ratio == "adaptive" {
			return inference.NewError(
				inference.InvalidExtension, field("aspect_ratio"),
				fmt.Errorf("minimax: text-to-video requires an explicit ratio; adaptive is not allowed"))
		}
		wire.ratio = ratio
	} else if textOnly {
		wire.ratio = videoRatioTextDefault
	}
	return nil
}

// videoCreateResponse is the v2 create-task envelope. The v2 API answers failures
// with HTTP status codes, but it may still describe them with the same base_resp
// envelope the v1 surfaces always send, so both are read: a 200 carrying a
// non-zero status is a refusal, not a task.
type videoCreateResponse struct {
	TaskID   string   `json:"task_id"`
	BaseResp baseResp `json:"base_resp"`
}

// videoQueryResponse is the v2 task-status envelope: the artifact URL rides
// content.url and the accounting rides task.usage.
type videoQueryResponse struct {
	Task struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Content struct {
			URL string `json:"url"`
		} `json:"content"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	} `json:"task"`
	BaseResp baseResp `json:"base_resp"`
}

// transportVideo creates the task, polls it to a terminal state, and returns the
// artifact URL.
func transportVideo(
	client *mediaClient,
) protocol.Transport[inference.GenerateRequest, videoWire, videoRaw] {
	return func(
		ctx context.Context,
		_ inference.GenerateRequest,
		wire videoWire,
	) (videoRaw, error) {
		var created videoCreateResponse
		if err := client.postJSON(ctx, videoPathCreate, videoTaskRequest(wire), &created); err != nil {
			return videoRaw{}, err
		}
		if err := created.BaseResp.err("video task creation"); err != nil {
			return videoRaw{}, err
		}
		if created.TaskID == "" {
			return videoRaw{}, inference.NewError(
				inference.InvalidProviderResponse, inference.FieldOutput,
				fmt.Errorf("minimax: video task creation returned no task_id"))
		}
		queryPath := videoPathQueryPrefix + url.PathEscape(created.TaskID)
		for {
			var query videoQueryResponse
			if err := client.getJSON(ctx, queryPath, &query); err != nil {
				return videoRaw{}, err
			}
			if err := query.BaseResp.err("video task query"); err != nil {
				return videoRaw{}, err
			}
			if query.Task.Status == "" {
				// A statusless task is a decode failure, not something to wait
				// on: polling it would spin until the caller's deadline and then
				// report an interruption instead of the provider's answer.
				return videoRaw{}, inference.NewError(
					inference.InvalidProviderResponse, inference.FieldOutput,
					fmt.Errorf("minimax: video task %q query carried no status", created.TaskID))
			}
			switch strings.ToLower(query.Task.Status) {
			case "succeeded":
				if query.Task.Content.URL == "" {
					return videoRaw{}, inference.NewError(
						inference.InvalidProviderResponse, inference.FieldOutput,
						fmt.Errorf("minimax: succeeded video task %q carries no content url",
							created.TaskID))
				}
				return videoRaw{
					videoURL:     query.Task.Content.URL,
					requestID:    created.TaskID,
					outputTokens: query.Task.Usage.CompletionTokens,
				}, nil
			case "failed":
				return videoRaw{}, errdefs.NotAvailable(fmt.Errorf(
					"minimax: video task %q failed server-side", created.TaskID))
			case "cancelled":
				return videoRaw{}, errdefs.NotAvailable(fmt.Errorf(
					"minimax: video task %q was cancelled server-side", created.TaskID))
			}
			select {
			case <-ctx.Done():
				return videoRaw{}, ctx.Err()
			case <-time.After(client.pollInterval()):
			}
		}
	}
}

// videoTaskBody is the v2 create payload. The required facts are always emitted;
// the optional switches disappear when unset, so a caller cannot accidentally send
// a zero the platform would read as an instruction.
type videoTaskBody struct {
	Model      string             `json:"model"`
	Content    []videoContentItem `json:"content"`
	Resolution string             `json:"resolution"`
	Duration   *int               `json:"duration,omitempty"`
	Ratio      string             `json:"ratio,omitempty"`
	Watermark  *bool              `json:"aigc_watermark,omitempty"`
	Callback   string             `json:"callback_url,omitempty"`
}

// videoContentItem is one entry of the v2 content array. Exactly one typed payload
// is set, and its JSON name is also the item's type, which is why the field is a
// struct rather than a dynamic key.
type videoContentItem struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *videoMediaURL `json:"image_url,omitempty"`
	VideoURL *videoMediaURL `json:"video_url,omitempty"`
	AudioURL *videoMediaURL `json:"audio_url,omitempty"`
	Role     string         `json:"role,omitempty"`
}

// videoMediaURL is the nested url object every media item carries.
type videoMediaURL struct {
	URL string `json:"url"`
}

// videoTaskRequest renders the v2 create payload: the mandatory text item first,
// then the media items with their roles.
func videoTaskRequest(wire videoWire) videoTaskBody {
	return videoTaskBody{
		Model:      wire.model,
		Content:    videoContentItems(wire),
		Resolution: wire.resolution,
		Duration:   wire.duration,
		Ratio:      wire.ratio,
		Watermark:  wire.watermark,
		Callback:   wire.callback,
	}
}

// videoContentItems renders the official v2 content array for a task.
func videoContentItems(wire videoWire) []videoContentItem {
	items := make([]videoContentItem, 0,
		1+len(wire.referenceImages)+len(wire.referenceVideos)+len(wire.referenceAudios))
	items = append(items, videoContentItem{Type: "text", Text: wire.prompt})
	if wire.firstFrame != "" {
		items = append(items, videoMediaItem("image_url", wire.firstFrame, "first_frame"))
	}
	if wire.lastFrame != "" {
		items = append(items, videoMediaItem("image_url", wire.lastFrame, "last_frame"))
	}
	for _, value := range wire.referenceImages {
		items = append(items, videoMediaItem("image_url", value, "reference_image"))
	}
	for _, value := range wire.referenceVideos {
		items = append(items, videoMediaItem("video_url", value, "reference_video"))
	}
	for _, value := range wire.referenceAudios {
		items = append(items, videoMediaItem("audio_url", value, "reference_audio"))
	}
	return items
}

// videoMediaItem builds one media entry with the role the task API expects.
func videoMediaItem(kind, value, role string) videoContentItem {
	item := videoContentItem{Type: kind, Role: role}
	media := &videoMediaURL{URL: value}
	switch kind {
	case "image_url":
		item.ImageURL = media
	case "video_url":
		item.VideoURL = media
	case "audio_url":
		item.AudioURL = media
	}
	return item
}

func decodeVideo(
	_ context.Context,
	request inference.GenerateRequest,
	raw videoRaw,
) (inference.GenerateResponse, error) {
	// MiniMax video tasks deliver mp4 files; the URL carries no explicit media
	// type, so the container the API produces is the truthful one.
	source, err := media.NewVideoURL(raw.videoURL, "video/mp4")
	if err != nil {
		return inference.GenerateResponse{}, fmt.Errorf("minimax: video url: %w", err)
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{message.VideoPart{Source: source}}},
		},
		FinishReason: inference.FinishStop,
		Usage: usage.Usage{
			OutputTokens: raw.outputTokens,
			Work:         map[usage.WorkKind]int64{usage.WorkVideosGenerated: 1},
		},
		Metadata: inference.Metadata{Model: request.Model.ID, RequestID: raw.requestID},
	}, nil
}

// imageSourceURI renders an input image the way the task API expects: absolute
// URLs pass through, inline bytes become a data URI carrying the source media
// type.
func imageSourceURI(source media.ImageSource, declared ModelSpec) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return inlineMediaURI(declared, message.PartImage, source.MediaType(), source.Bytes())
}

func videoSourceURI(source media.VideoSource, declared ModelSpec) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return inlineMediaURI(declared, message.PartVideo, source.MediaType(), source.Bytes())
}

func audioSourceURI(source media.AudioSource, declared ModelSpec) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return inlineMediaURI(declared, message.PartAudio, source.MediaType(), source.Bytes())
}

// inlineMediaURI renders an inline payload as a data URI, rejecting one above the
// model's declared byte ceiling. The ceiling is enforced here rather than left to
// the endpoint because Base64 inflates the payload by a third and the marshalled
// body holds another copy, so an oversized input costs several times its own size
// — per part, before anything is sent. An undeclared ceiling filters nothing,
// which is the documented meaning of an undeclared limit.
//
// The prefix and the encoded payload go into one buffer sized up front: encoding
// to a string and concatenating it would hold the payload twice more on a path
// that already pays a copy for the source bytes.
func inlineMediaURI(
	declared ModelSpec,
	kind message.PartKind,
	mediaType string,
	data []byte,
) (string, error) {
	if limit := declared.Limits.MaxInputBytes; limit != nil && int64(len(data)) > *limit {
		return "", inference.NewError(
			inference.InvalidRequest, inference.FieldContent,
			fmt.Errorf(
				"minimax: %s %s input is %d bytes, above the declared max_input_bytes of %d",
				declared.Name, kind, len(data), *limit))
	}
	const prefix = "data:"
	const marker = ";base64,"
	header := len(prefix) + len(mediaType) + len(marker)
	buffer := make([]byte, header, header+base64.StdEncoding.EncodedLen(len(data)))
	copy(buffer, prefix)
	copy(buffer[len(prefix):], mediaType)
	copy(buffer[len(prefix)+len(mediaType):], marker)
	buffer = base64.StdEncoding.AppendEncode(buffer, data)
	return string(buffer), nil
}
