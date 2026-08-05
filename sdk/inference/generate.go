package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// GenerateExecutionShape identifies the provider execution contract being
// compiled. A provider may support different canonical fields for unary and
// streaming generation.
type GenerateExecutionShape string

const (
	GenerateExecutionUnary  GenerateExecutionShape = "unary"
	GenerateExecutionStream GenerateExecutionShape = "stream"
)

func (s GenerateExecutionShape) Validate() error {
	switch s {
	case GenerateExecutionUnary, GenerateExecutionStream:
		return nil
	default:
		return fmt.Errorf("unknown generate execution shape %q", s)
	}
}

func (s GenerateExecutionShape) Field() FieldID {
	switch s {
	case GenerateExecutionUnary:
		return FieldGenerateExecutionUnary
	case GenerateExecutionStream:
		return FieldGenerateExecutionStream
	default:
		return ""
	}
}

// GenerateCompiler compiles a canonical Generate request for one explicit
// execution shape without provider I/O.
type GenerateCompiler[Wire any] func(
	context.Context,
	ModelRef,
	GenerateRequest,
	GenerateExecutionShape,
) (Compiled[Wire], error)

// InputRole is deliberately narrower than [message.Message].Role: only a user turn or a
// tool continuation may be the one current input to Generate.
type InputRole string

const (
	InputRoleUser InputRole = "user"
	InputRoleTool InputRole = "tool"
)

type GenerateInput struct {
	Role    InputRole    `json:"role"`
	Content InputContent `json:"content"`
}

func (i GenerateInput) Clone() GenerateInput {
	i.Content = i.Content.Clone()
	return i
}

func (i GenerateInput) Validate() error {
	var role message.Role
	switch i.Role {
	case InputRoleUser:
		role = message.RoleUser
	case InputRoleTool:
		role = message.RoleTool
	default:
		return fmt.Errorf("generate input role must be user or tool")
	}
	if err := i.Content.Validate(); err != nil {
		return err
	}
	return (message.Message{Role: role, Content: i.Content.Content}).Validate()
}

// Message converts an executed input into ordinary history. The returned
// message owns a clone of the parts and cannot retain Intent by construction.
func (i GenerateInput) Message() message.Message {
	return message.Message{Role: message.Role(i.Role), Content: i.Content.Content.Clone()}
}

type GenerateRequest struct {
	Context    []message.Message `json:"context,omitempty" ledger:"generate.context.*.role"`
	Input      GenerateInput     `json:"input" ledger:"generate.input.role"`
	Extensions Extensions        `json:"-" ledger:"extension"`
}

func (r GenerateRequest) Clone() GenerateRequest {
	clone := r
	clone.Context = make([]message.Message, len(r.Context))
	for i, message := range r.Context {
		clone.Context[i] = message.Clone()
	}
	clone.Input = r.Input.Clone()
	clone.Extensions = r.Extensions.Clone()
	return clone
}

func (r GenerateRequest) Validate() error {
	for i, message := range r.Context {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("context message %d: %w", i, err)
		}
	}
	if err := r.Input.Validate(); err != nil {
		return fmt.Errorf("generate input: %w", err)
	}
	return r.Extensions.Validate()
}

func (r GenerateRequest) ActiveFields() []FieldID {
	var fields []FieldID
	if len(r.Context) > 0 {
		fields = append(fields, FieldGenerateContextRole)
		fields = appendGenerateContextPartFields(fields, r.Context)
	}
	if r.Input.Role != "" {
		fields = append(fields, FieldGenerateInputRole)
	}
	fields = appendGenerateInputPartFields(fields, r.Input.Content.Parts)
	fields = appendGenerateIntentFields(fields, r.Input.Content.Intent)
	return r.Extensions.AppendActiveFields(fields)
}

// ActiveFieldsFor returns the complete Generate ledger for one execution
// shape, including the shape itself.
func (r GenerateRequest) ActiveFieldsFor(shape GenerateExecutionShape) []FieldID {
	fields := r.ActiveFields()
	if field := shape.Field(); field != "" {
		fields = append(fields, field)
	}
	return fields
}

func appendGenerateContextPartFields(
	fields []FieldID,
	messages []message.Message,
) []FieldID {
	seen := make(map[message.PartKind]bool)
	for _, message := range messages {
		for _, part := range message.Content.Parts {
			if part != nil {
				seen[part.Kind()] = true
			}
		}
	}
	for _, item := range []struct {
		kind  message.PartKind
		field FieldID
	}{
		{message.PartText, FieldGenerateContextText},
		{message.PartImage, FieldGenerateContextImage},
		{message.PartAudio, FieldGenerateContextAudio},
		{message.PartVideo, FieldGenerateContextVideo},
		{message.PartFile, FieldGenerateContextFile},
		{message.PartData, FieldGenerateContextData},
		{message.PartToolCall, FieldGenerateContextToolCall},
		{message.PartToolResult, FieldGenerateContextToolResult},
		{message.PartReasoning, FieldGenerateContextReasoning},
	} {
		if seen[item.kind] {
			fields = append(fields, item.field)
		}
	}
	return fields
}

func appendGenerateInputPartFields(fields []FieldID, parts []message.Part) []FieldID {
	seen := make(map[message.PartKind]bool)
	for _, part := range parts {
		if part != nil {
			seen[part.Kind()] = true
		}
	}
	for _, item := range []struct {
		kind  message.PartKind
		field FieldID
	}{
		{message.PartText, FieldGenerateInputText},
		{message.PartImage, FieldGenerateInputImage},
		{message.PartAudio, FieldGenerateInputAudio},
		{message.PartVideo, FieldGenerateInputVideo},
		{message.PartFile, FieldGenerateInputFile},
		{message.PartData, FieldGenerateInputData},
		{message.PartToolCall, FieldGenerateInputToolCall},
		{message.PartToolResult, FieldGenerateInputToolResult},
		{message.PartReasoning, FieldGenerateInputReasoning},
	} {
		if seen[item.kind] {
			fields = append(fields, item.field)
		}
	}
	return fields
}

func appendGenerateIntentFields(fields []FieldID, intent Intent) []FieldID {
	if intent.Text != nil {
		fields = append(fields, FieldGenerateIntentText)
		if intent.Text.Response != nil {
			fields = append(fields, FieldGenerateIntentTextResponse)
			if intent.Text.Response.Kind != "" {
				fields = append(fields, FieldGenerateIntentTextResponseKind)
			}
			if intent.Text.Response.Name != "" {
				fields = append(fields, FieldGenerateIntentTextResponseName)
			}
			if len(intent.Text.Response.Schema) > 0 {
				fields = append(fields, FieldGenerateIntentTextResponseSchema)
			}
		}
		if intent.Text.MaxOutputTokens != nil {
			fields = append(fields, FieldGenerateIntentTextMaxOutputTokens)
		}
		if len(intent.Text.Tools) > 0 {
			fields = append(fields, FieldGenerateIntentTools)
		}
		if intent.Text.ToolChoice != nil {
			fields = append(fields, FieldGenerateIntentToolChoice)
			if intent.Text.ToolChoice.Kind != "" {
				fields = append(fields, FieldGenerateIntentToolChoiceKind)
			}
			if intent.Text.ToolChoice.Name != "" {
				fields = append(fields, FieldGenerateIntentToolChoiceName)
			}
		}
		if intent.Text.Temperature != nil {
			fields = append(fields, FieldGenerateIntentTemperature)
		}
		if intent.Text.TopP != nil {
			fields = append(fields, FieldGenerateIntentTopP)
		}
		if intent.Text.ReasoningEnabled != nil || intent.Text.ReasoningEffort != "" {
			fields = append(fields, FieldGenerateIntentReasoning)
		}
		if intent.Text.ReasoningEnabled != nil {
			fields = append(fields, FieldGenerateIntentReasoningEnabled)
		}
		if intent.Text.ReasoningEffort != "" {
			fields = append(fields, FieldGenerateIntentReasoningEffort)
		}
	}
	if intent.Image != nil {
		fields = append(fields, FieldGenerateIntentImage)
		if intent.Image.Size != nil {
			fields = append(
				fields,
				FieldGenerateIntentImageSize,
				FieldGenerateIntentImageSizeWidth,
				FieldGenerateIntentImageSizeHeight,
			)
		}
		if intent.Image.AspectRatio != "" {
			fields = append(fields, FieldGenerateIntentImageAspectRatio)
		}
		if intent.Image.Count != nil {
			fields = append(fields, FieldGenerateIntentImageCount)
		}
		if intent.Image.Seed != nil {
			fields = append(fields, FieldGenerateIntentImageSeed)
		}
		if intent.Image.OutputFormat != "" {
			fields = append(fields, FieldGenerateIntentImageOutputFormat)
		}
		if intent.Image.Delivery != "" {
			fields = append(fields, FieldGenerateIntentImageDelivery)
		}
	}
	if intent.Audio != nil {
		fields = append(fields, FieldGenerateIntentAudio, FieldGenerateIntentAudioVoice)
		if intent.Audio.Voice.ID != "" {
			fields = append(fields, FieldGenerateIntentAudioVoiceID)
		}
		if intent.Audio.Voice.Language != "" {
			fields = append(fields, FieldGenerateIntentAudioVoiceLanguage)
		}
		fields = append(fields, FieldGenerateIntentAudioFormat)
		if intent.Audio.Format.Encoding != "" {
			fields = append(fields, FieldGenerateIntentAudioFormatEncoding)
		}
		if intent.Audio.Format.SampleRateHz != 0 {
			fields = append(fields, FieldGenerateIntentAudioFormatSampleRate)
		}
		if intent.Audio.Format.Channels != 0 {
			fields = append(fields, FieldGenerateIntentAudioFormatChannels)
		}
		if intent.Audio.Speed != nil {
			fields = append(fields, FieldGenerateIntentAudioSpeed)
		}
		if intent.Audio.Count != nil {
			fields = append(fields, FieldGenerateIntentAudioCount)
		}
	}
	if intent.Video != nil {
		fields = append(fields, FieldGenerateIntentVideo)
		if intent.Video.DurationMillis != nil {
			fields = append(fields, FieldGenerateIntentVideoDuration)
		}
		if intent.Video.Resolution != "" {
			fields = append(fields, FieldGenerateIntentVideoResolution)
		}
		if intent.Video.AspectRatio != "" {
			fields = append(fields, FieldGenerateIntentVideoAspectRatio)
		}
		if intent.Video.Seed != nil {
			fields = append(fields, FieldGenerateIntentVideoSeed)
		}
		if intent.Video.Watermark != nil {
			fields = append(fields, FieldGenerateIntentVideoWatermark)
		}
	}
	return fields
}

// Intent declares the output modalities one generation should produce.
// Controls are not free-floating: they live on the modality they govern,
// so combinations no provider can honor (image with a temperature, video
// with tool calls) are unrepresentable rather than rejected at runtime.
type Intent struct {
	Text  *TextIntent  `json:"text,omitempty"`
	Image *ImageIntent `json:"image,omitempty"`
	Audio *AudioIntent `json:"audio,omitempty"`
	Video *VideoIntent `json:"video,omitempty"`
}

func (i Intent) Clone() Intent {
	clone := i
	if i.Text != nil {
		value := i.Text.Clone()
		clone.Text = &value
	}
	if i.Image != nil {
		value := i.Image.Clone()
		clone.Image = &value
	}
	if i.Audio != nil {
		value := i.Audio.Clone()
		clone.Audio = &value
	}
	if i.Video != nil {
		value := i.Video.Clone()
		clone.Video = &value
	}
	return clone
}

func (i Intent) Validate() error {
	if i.Text == nil && i.Image == nil && i.Audio == nil && i.Video == nil {
		return fmt.Errorf("intent requires text, image, audio, or video output")
	}
	for name, modality := range map[string]interface{ Validate() error }{
		"text": i.Text, "image": i.Image, "audio": i.Audio, "video": i.Video,
	} {
		if !isNilValue(modality) {
			if err := modality.Validate(); err != nil {
				return fmt.Errorf("%s intent: %w", name, err)
			}
		}
	}
	return nil
}

// TextIntent declares text output and owns every control that governs
// text generation: response shaping, tool calling, sampling, and the
// reasoning trace. A TextIntent carrying only tools is the tools-first
// shape — text output stays welcome because the modality itself is
// present.
type TextIntent struct {
	Response        *ResponseFormat `json:"response,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	// Tools lists the tool definitions the model may call; ToolChoice
	// constrains when and which. Either one marks tool calling as
	// requested.
	Tools      []message.Definition `json:"tools,omitempty"`
	ToolChoice *ToolChoice          `json:"tool_choice,omitempty"`
	// Sampling controls: temperature in [0, 2], top_p in [0, 1].
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	// ReasoningEnabled is the universal reasoning switch — every
	// provider can turn thinking on or off, so the compiler honors it
	// exactly (or rejects where a model cannot comply). ReasoningEffort
	// tunes depth where the platform has levels; platforms whose
	// thinking is binary quantize it, and the compiler reports the loss.
	ReasoningEnabled *bool           `json:"reasoning_enabled,omitempty"`
	ReasoningEffort  ReasoningEffort `json:"reasoning_effort,omitempty"`
}

// toolsRequested reports whether tool calling was requested: definitions
// or a choice.
func (i TextIntent) toolsRequested() bool {
	return len(i.Tools) > 0 || i.ToolChoice != nil
}

func (i TextIntent) Clone() TextIntent {
	if i.Response != nil {
		response := *i.Response
		response.Schema = json.RawMessage(append([]byte(nil), response.Schema...))
		i.Response = &response
	}
	i.MaxOutputTokens = clonePointer(i.MaxOutputTokens)
	if i.Tools != nil {
		tools := make([]message.Definition, len(i.Tools))
		for index, definition := range i.Tools {
			tools[index] = definition.Clone()
		}
		i.Tools = tools
	}
	i.ToolChoice = clonePointer(i.ToolChoice)
	i.Temperature = clonePointer(i.Temperature)
	i.TopP = clonePointer(i.TopP)
	i.ReasoningEnabled = clonePointer(i.ReasoningEnabled)
	return i
}

func (i TextIntent) Validate() error {
	if i.Response != nil {
		if err := i.Response.Validate(); err != nil {
			return err
		}
	}
	if i.MaxOutputTokens != nil && *i.MaxOutputTokens <= 0 {
		return fmt.Errorf("max output tokens must be positive")
	}
	names := make(map[string]struct{}, len(i.Tools))
	for _, definition := range i.Tools {
		if err := definition.Validate(); err != nil {
			return err
		}
		if _, exists := names[definition.Name]; exists {
			return fmt.Errorf("duplicate tool definition %q", definition.Name)
		}
		names[definition.Name] = struct{}{}
	}
	if i.ToolChoice != nil {
		if err := i.ToolChoice.Validate(); err != nil {
			return err
		}
		if len(i.Tools) == 0 && i.ToolChoice.Kind != ToolChoiceNone {
			return fmt.Errorf("tool choice requires tool definitions")
		}
		if i.ToolChoice.Kind == ToolChoiceNamed {
			if _, exists := names[i.ToolChoice.Name]; !exists {
				return fmt.Errorf("named tool choice %q is not defined", i.ToolChoice.Name)
			}
		}
	}
	if i.Temperature != nil &&
		(math.IsNaN(*i.Temperature) || math.IsInf(*i.Temperature, 0) ||
			*i.Temperature < 0 || *i.Temperature > 2) {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	if i.TopP != nil &&
		(math.IsNaN(*i.TopP) || math.IsInf(*i.TopP, 0) ||
			*i.TopP < 0 || *i.TopP > 1) {
		return fmt.Errorf("top_p must be between 0 and 1")
	}
	switch i.ReasoningEffort {
	case "", ReasoningLow, ReasoningMedium, ReasoningHigh:
	default:
		return fmt.Errorf("unknown reasoning effort %q", i.ReasoningEffort)
	}
	if i.ReasoningEnabled != nil && !*i.ReasoningEnabled && i.ReasoningEffort != "" {
		return fmt.Errorf("reasoning cannot be disabled while an effort is requested")
	}
	return nil
}

type ImageIntent struct {
	Size         *media.ImageSize  `json:"size,omitempty"`
	AspectRatio  media.AspectRatio `json:"aspect_ratio,omitempty"`
	Count        *int              `json:"count,omitempty"`
	Seed         *int64            `json:"seed,omitempty"`
	OutputFormat media.ImageFormat `json:"output_format,omitempty"`
	Delivery     media.SourceKind  `json:"delivery,omitempty"`
}

func (i ImageIntent) Clone() ImageIntent {
	i.Size = clonePointer(i.Size)
	i.Count = clonePointer(i.Count)
	i.Seed = clonePointer(i.Seed)
	return i
}

func (i ImageIntent) Validate() error {
	if i.Size != nil && i.AspectRatio != "" {
		return fmt.Errorf("image size and aspect ratio are mutually exclusive")
	}
	if i.Size != nil {
		if err := i.Size.Validate(); err != nil {
			return err
		}
	}
	if i.AspectRatio != "" {
		if err := i.AspectRatio.Validate(); err != nil {
			return err
		}
	}
	if i.Count != nil && *i.Count <= 0 {
		return fmt.Errorf("image count must be positive")
	}
	if i.OutputFormat != "" {
		if err := i.OutputFormat.Validate(); err != nil {
			return err
		}
	}
	if i.Delivery != "" {
		if err := i.Delivery.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type AudioIntent struct {
	// Voice selects the synthesis voice. It is optional at the canonical
	// layer: speech models require one (their compilers reject a missing
	// voice), while voice-free synthesis such as music generation omits
	// it.
	Voice  media.VoiceSpec   `json:"voice,omitempty"`
	Format media.AudioFormat `json:"format"`
	Speed  *float64          `json:"speed,omitempty"`
	Count  *int              `json:"count,omitempty"`
}

func (i AudioIntent) Clone() AudioIntent {
	i.Speed = clonePointer(i.Speed)
	i.Count = clonePointer(i.Count)
	return i
}

func (i AudioIntent) Validate() error {
	if i.Voice.ID != "" || i.Voice.Language != "" {
		if err := i.Voice.Validate(); err != nil {
			return err
		}
	}
	if err := i.Format.Validate(); err != nil {
		return err
	}
	if i.Speed != nil &&
		(math.IsNaN(*i.Speed) || math.IsInf(*i.Speed, 0) ||
			*i.Speed < 0.25 || *i.Speed > 4) {
		return fmt.Errorf("audio speed must be between 0.25 and 4")
	}
	if i.Count != nil && *i.Count <= 0 {
		return fmt.Errorf("audio count must be positive")
	}
	return nil
}

// VideoIntent requests video generation. Providers typically run video
// synthesis as a long task behind the scenes; the unary contract still
// applies — the provider must complete within the caller's context
// deadline. Videos are all-or-nothing: there is no count knob, and a
// completed response carries at least one message.VideoPart.
type VideoIntent struct {
	DurationMillis *int64            `json:"duration_millis,omitempty"`
	Resolution     string            `json:"resolution,omitempty"`
	AspectRatio    media.AspectRatio `json:"aspect_ratio,omitempty"`
	Seed           *int64            `json:"seed,omitempty"`
	Watermark      *bool             `json:"watermark,omitempty"`
}

func (i VideoIntent) Clone() VideoIntent {
	i.DurationMillis = clonePointer(i.DurationMillis)
	i.Seed = clonePointer(i.Seed)
	i.Watermark = clonePointer(i.Watermark)
	return i
}

var videoResolutionPattern = regexp.MustCompile(`^[0-9]+[pPkK]$`)

func (i VideoIntent) Validate() error {
	if i.DurationMillis != nil && *i.DurationMillis <= 0 {
		return fmt.Errorf("video duration must be positive")
	}
	if i.Resolution != "" && !videoResolutionPattern.MatchString(i.Resolution) {
		return fmt.Errorf(
			"video resolution must be a tier token like \"720p\" or \"4k\", not %q",
			i.Resolution,
		)
	}
	if i.AspectRatio != "" {
		if err := i.AspectRatio.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type GenerateResponse struct {
	Message      message.Message `json:"message"`
	FinishReason FinishReason    `json:"finish_reason"`
	Usage        Usage           `json:"usage"`
	Metadata     Metadata        `json:"metadata"`
}

func (r GenerateResponse) Clone() GenerateResponse {
	r.Message = r.Message.Clone()
	r.Usage = r.Usage.Clone()
	r.Metadata.Decisions = append([]Decision(nil), r.Metadata.Decisions...)
	return r
}

func (r GenerateResponse) Validate() error {
	if r.Message.Role != message.RoleAssistant {
		return fmt.Errorf("generate response message must have assistant role")
	}
	if err := r.FinishReason.Validate(); err != nil {
		return err
	}
	if len(r.Message.Content.Parts) == 0 {
		if r.FinishReason == FinishCompleted || r.FinishReason == FinishToolCalls {
			return fmt.Errorf("generate response content is required for finish reason %q", r.FinishReason)
		}
	} else if err := r.Message.Validate(); err != nil {
		return err
	}
	if err := r.Usage.Validate(); err != nil {
		return err
	}
	hasToolCalls := r.Message.HasToolCalls()
	if (r.FinishReason == FinishToolCalls) != hasToolCalls {
		return fmt.Errorf("tool-call finish reason does not match response tool calls")
	}
	for _, part := range r.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return err
		}
		switch normalized.(type) {
		case message.TextPart, message.ImagePart, message.AudioPart, message.VideoPart, message.ToolCallPart, message.ReasoningPart:
		default:
			return fmt.Errorf("generate response contains unsupported part %q", normalized.Kind())
		}
	}
	return nil
}

func (r GenerateResponse) ValidateFor(request GenerateRequest) error {
	deriveGenerateUsage(request, &r)
	if err := r.Validate(); err != nil {
		return err
	}
	intent := request.Input.Content.Intent
	toolsRequested := intent.Text != nil && intent.Text.toolsRequested()
	var text strings.Builder
	textParts := 0
	var images []message.ImagePart
	var audio []message.AudioPart
	var videos []message.VideoPart
	var toolCalls []message.ToolCallPart
	for _, part := range r.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return err
		}
		switch value := normalized.(type) {
		case message.TextPart:
			if intent.Text == nil {
				return fmt.Errorf("generate response contains unrequested text")
			}
			textParts++
			text.WriteString(value.Text)
		case message.ImagePart:
			if intent.Image == nil {
				return fmt.Errorf("generate response contains unrequested image")
			}
			images = append(images, value)
			if err := validateGenerateImage(value, *intent.Image); err != nil {
				return fmt.Errorf("generate image %d: %w", len(images)-1, err)
			}
		case message.AudioPart:
			if intent.Audio == nil {
				return fmt.Errorf("generate response contains unrequested audio")
			}
			audio = append(audio, value)
			if err := validateGenerateAudio(value, *intent.Audio); err != nil {
				return fmt.Errorf("generate audio %d: %w", len(audio)-1, err)
			}
		case message.VideoPart:
			if intent.Video == nil {
				return fmt.Errorf("generate response contains unrequested video")
			}
			videos = append(videos, value)
			if err := validateGenerateVideo(value); err != nil {
				return fmt.Errorf("generate video %d: %w", len(videos)-1, err)
			}
		case message.ToolCallPart:
			if !toolsRequested {
				return fmt.Errorf("generate response contains an unrequested tool call")
			}
			toolCalls = append(toolCalls, value)
		case message.ReasoningPart:
			// Reasoning is a trace of the model's own process, not a
			// requested artifact: reasoning-capable models emit it whether
			// or not the request set a reasoning intent, so responses may
			// always carry it.
		}
	}
	if toolsRequested {
		definitions := make(map[string]struct{}, len(intent.Text.Tools))
		for _, definition := range intent.Text.Tools {
			definitions[definition.Name] = struct{}{}
		}
		for index, call := range toolCalls {
			if _, ok := definitions[call.Call.Name]; !ok {
				return fmt.Errorf("generate tool call %d names undefined tool %q", index, call.Call.Name)
			}
		}
		if choice := intent.Text.ToolChoice; choice != nil {
			switch choice.Kind {
			case ToolChoiceNone:
				if len(toolCalls) != 0 {
					return fmt.Errorf("tool choice none forbids tool calls")
				}
			case ToolChoiceRequired:
				if len(toolCalls) == 0 && r.FinishReason == FinishCompleted {
					return fmt.Errorf("required tool choice produced no tool call")
				}
			case ToolChoiceNamed:
				for _, call := range toolCalls {
					if call.Call.Name != choice.Name {
						return fmt.Errorf(
							"named tool choice %q produced tool %q",
							choice.Name,
							call.Call.Name,
						)
					}
				}
				if len(toolCalls) == 0 && r.FinishReason == FinishCompleted {
					return fmt.Errorf("named tool choice produced no tool call")
				}
			}
		}
	}
	if r.FinishReason != FinishCompleted {
		return nil
	}
	if intent.Text != nil {
		if textParts == 0 {
			return fmt.Errorf("completed generate response contains no requested text")
		}
		if err := validateGenerateText(text.String(), intent.Text.Response); err != nil {
			return err
		}
	}
	if intent.Image != nil {
		if err := validateGenerateCount("image", len(images), intent.Image.Count); err != nil {
			return err
		}
	}
	if intent.Audio != nil {
		if err := validateGenerateCount("audio", len(audio), intent.Audio.Count); err != nil {
			return err
		}
	}
	if intent.Video != nil {
		if err := validateGenerateCount("video", len(videos), nil); err != nil {
			return err
		}
	}
	return nil
}

// validateGenerateVideo checks the output part is genuinely video. Unlike
// image and audio, no canonical video parameter constrains the returned
// encoding beyond the media family.
func validateGenerateVideo(part message.VideoPart) error {
	if mediaType := part.Source.BaseMediaType(); !strings.HasPrefix(mediaType, "video/") {
		return fmt.Errorf("video part media type %q is not video", part.Source.MediaType())
	}
	return nil
}

func deriveGenerateUsage(request GenerateRequest, response *GenerateResponse) {
	imageCount := int64(0)
	hasImage := false
	hasAudio := false
	audioDurationKnown := true
	audioDuration := int64(0)
	videoCount := int64(0)
	hasVideo := false
	for _, part := range response.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		switch value := normalized.(type) {
		case message.ImagePart:
			hasImage = true
			imageCount++
		case message.AudioPart:
			hasAudio = true
			if value.DurationMillis == nil {
				audioDurationKnown = false
			} else {
				audioDuration += *value.DurationMillis
			}
		case message.VideoPart:
			hasVideo = true
			videoCount++
		}
	}
	if hasImage || request.Input.Content.Intent.Image != nil {
		response.Usage.GeneratedImages = &imageCount
	} else {
		response.Usage.GeneratedImages = nil
	}
	if hasAudio && audioDurationKnown {
		response.Usage.AudioDurationMillis = &audioDuration
	} else {
		response.Usage.AudioDurationMillis = nil
	}
	if hasVideo || request.Input.Content.Intent.Video != nil {
		response.Usage.GeneratedVideos = &videoCount
	} else {
		response.Usage.GeneratedVideos = nil
	}
}

func validateGenerateCount(name string, actual int, requested *int) error {
	if requested == nil {
		if actual == 0 {
			return fmt.Errorf("completed generate response contains no requested %s", name)
		}
		return nil
	}
	if actual != *requested {
		return fmt.Errorf(
			"generate %s count %d does not match requested count %d",
			name,
			actual,
			*requested,
		)
	}
	return nil
}

func validateGenerateImage(part message.ImagePart, intent ImageIntent) error {
	if intent.Delivery != "" && part.Source.Kind() != intent.Delivery {
		return fmt.Errorf(
			"delivery %q does not match requested delivery %q",
			part.Source.Kind(),
			intent.Delivery,
		)
	}
	if intent.OutputFormat != "" &&
		part.Source.BaseMediaType() != intent.OutputFormat.MediaType() {
		return fmt.Errorf(
			"media type %q does not match requested format %q",
			part.Source.MediaType(),
			intent.OutputFormat,
		)
	}
	return nil
}

func validateGenerateAudio(part message.AudioPart, intent AudioIntent) error {
	if part.Format == nil {
		return fmt.Errorf("audio format is required")
	}
	if part.Format.Encoding != intent.Format.Encoding {
		return fmt.Errorf("audio encoding does not match requested format")
	}
	if intent.Format.SampleRateHz != 0 &&
		part.Format.SampleRateHz != intent.Format.SampleRateHz {
		return fmt.Errorf("audio sample rate does not match requested format")
	}
	if intent.Format.Channels != 0 && part.Format.Channels != intent.Format.Channels {
		return fmt.Errorf("audio channels do not match requested format")
	}
	return nil
}

func validateGenerateText(output string, format *ResponseFormat) error {
	if format == nil || format.Kind == ResponseText {
		return nil
	}
	var value any
	if err := decodeStrict([]byte(output), &value); err != nil {
		return fmt.Errorf("structured generate response is not valid JSON: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("structured generate response must be a JSON object")
	}
	if format.Kind != ResponseJSONSchema {
		return nil
	}
	compiler := newInMemoryJSONSchemaCompiler()
	const resource = "inference://generate-response-schema.json"
	if err := compiler.AddResource(resource, bytes.NewReader(format.Schema)); err != nil {
		return fmt.Errorf("load generate response JSON schema: %w", err)
	}
	schema, err := compiler.Compile(resource)
	if err != nil {
		return fmt.Errorf("compile generate response JSON schema: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("generate response does not match requested JSON schema: %w", err)
	}
	return nil
}
