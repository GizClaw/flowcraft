package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/media"
	"github.com/GizClaw/flowcraft/sdk/tool"
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

// InputRole is deliberately narrower than Message Role: only a user turn or a
// tool continuation may be the one current input to Generate.
type InputRole string

const (
	InputRoleUser InputRole = "user"
	InputRoleTool InputRole = "tool"
)

type InputContent struct {
	Content
	Intent Intent `json:"intent"`
}

// MarshalJSON is explicit because Content implements json.Marshaler; without
// this method the embedded marshaler would otherwise hide Intent.
func (c InputContent) MarshalJSON() ([]byte, error) {
	contentData, err := json.Marshal(c.Content)
	if err != nil {
		return nil, err
	}
	var content struct {
		Parts json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(contentData, &content); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Parts  json.RawMessage `json:"parts"`
		Intent Intent          `json:"intent"`
	}{Parts: content.Parts, Intent: c.Intent})
}

func (c *InputContent) UnmarshalJSON(data []byte) error {
	var wire struct {
		Parts  json.RawMessage `json:"parts"`
		Intent Intent          `json:"intent"`
	}
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	contentData, err := json.Marshal(struct {
		Parts json.RawMessage `json:"parts"`
	}{Parts: wire.Parts})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(contentData, &c.Content); err != nil {
		return err
	}
	c.Intent = wire.Intent
	return nil
}

func (c InputContent) Clone() InputContent {
	c.Content = c.Content.Clone()
	c.Intent = c.Intent.Clone()
	return c
}

func (c InputContent) Validate() error {
	if err := c.Content.Validate(); err != nil {
		return err
	}
	return c.Intent.Validate()
}

type GenerateInput struct {
	Role    InputRole    `json:"role"`
	Content InputContent `json:"content"`
}

func (i GenerateInput) Clone() GenerateInput {
	i.Content = i.Content.Clone()
	return i
}

func (i GenerateInput) Validate() error {
	var role Role
	switch i.Role {
	case InputRoleUser:
		role = RoleUser
	case InputRoleTool:
		role = RoleTool
	default:
		return fmt.Errorf("generate input role must be user or tool")
	}
	if err := i.Content.Validate(); err != nil {
		return err
	}
	return (Message{Role: role, Content: i.Content.Content}).Validate()
}

// Message converts an executed input into ordinary history. The returned
// message owns a clone of the parts and cannot retain Intent by construction.
func (i GenerateInput) Message() Message {
	return Message{Role: Role(i.Role), Content: i.Content.Content.Clone()}
}

type GenerateRequest struct {
	Context    []Message     `json:"context,omitempty" ledger:"generate.context.*.role"`
	Input      GenerateInput `json:"input" ledger:"generate.input.role"`
	Extensions Extensions    `json:"-" ledger:"extension"`
}

func (r GenerateRequest) Clone() GenerateRequest {
	clone := r
	clone.Context = make([]Message, len(r.Context))
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
	messages []Message,
) []FieldID {
	seen := make(map[PartKind]bool)
	for _, message := range messages {
		for _, part := range message.Content.Parts {
			if part != nil {
				seen[part.Kind()] = true
			}
		}
	}
	for _, item := range []struct {
		kind  PartKind
		field FieldID
	}{
		{PartText, FieldGenerateContextText},
		{PartImage, FieldGenerateContextImage},
		{PartAudio, FieldGenerateContextAudio},
		{PartVideo, FieldGenerateContextVideo},
		{PartFile, FieldGenerateContextFile},
		{PartData, FieldGenerateContextData},
		{PartToolCall, FieldGenerateContextToolCall},
		{PartToolResult, FieldGenerateContextToolResult},
	} {
		if seen[item.kind] {
			fields = append(fields, item.field)
		}
	}
	return fields
}

func appendGenerateInputPartFields(fields []FieldID, parts []Part) []FieldID {
	seen := make(map[PartKind]bool)
	for _, part := range parts {
		if part != nil {
			seen[part.Kind()] = true
		}
	}
	for _, item := range []struct {
		kind  PartKind
		field FieldID
	}{
		{PartText, FieldGenerateInputText},
		{PartImage, FieldGenerateInputImage},
		{PartAudio, FieldGenerateInputAudio},
		{PartVideo, FieldGenerateInputVideo},
		{PartFile, FieldGenerateInputFile},
		{PartData, FieldGenerateInputData},
		{PartToolCall, FieldGenerateInputToolCall},
		{PartToolResult, FieldGenerateInputToolResult},
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
	if intent.Tools != nil {
		fields = append(fields, FieldGenerateIntentTools)
		if len(intent.Tools.Definitions) > 0 {
			fields = append(fields, FieldGenerateIntentToolDefinitions)
		}
		if intent.Tools.Choice != nil {
			fields = append(fields, FieldGenerateIntentToolChoice)
			if intent.Tools.Choice.Kind != "" {
				fields = append(fields, FieldGenerateIntentToolChoiceKind)
			}
			if intent.Tools.Choice.Name != "" {
				fields = append(fields, FieldGenerateIntentToolChoiceName)
			}
		}
	}
	if intent.Sampling != nil {
		fields = append(fields, FieldGenerateIntentSampling)
		if intent.Sampling.Temperature != nil {
			fields = append(fields, FieldGenerateIntentSamplingTemperature)
		}
		if intent.Sampling.TopP != nil {
			fields = append(fields, FieldGenerateIntentSamplingTopP)
		}
	}
	if intent.Reasoning != nil {
		fields = append(fields, FieldGenerateIntentReasoning)
		if intent.Reasoning.Effort != "" {
			fields = append(fields, FieldGenerateIntentReasoningEffort)
		}
	}
	return fields
}

// Intent composes output modalities and cross-modal controls atomically.
type Intent struct {
	Text      *TextIntent      `json:"text,omitempty"`
	Image     *ImageIntent     `json:"image,omitempty"`
	Audio     *AudioIntent     `json:"audio,omitempty"`
	Video     *VideoIntent     `json:"video,omitempty"`
	Tools     *ToolsIntent     `json:"tools,omitempty"`
	Sampling  *SamplingIntent  `json:"sampling,omitempty"`
	Reasoning *ReasoningIntent `json:"reasoning,omitempty"`
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
	if i.Tools != nil {
		value := i.Tools.Clone()
		clone.Tools = &value
	}
	if i.Sampling != nil {
		value := i.Sampling.Clone()
		clone.Sampling = &value
	}
	clone.Reasoning = clonePointer(i.Reasoning)
	return clone
}

func (i Intent) Validate() error {
	if i.Text == nil && i.Image == nil && i.Audio == nil && i.Video == nil && i.Tools == nil {
		return fmt.Errorf("intent requires text, image, audio, video, or tools output")
	}
	for name, control := range map[string]interface{ Validate() error }{
		"text": i.Text, "image": i.Image, "audio": i.Audio, "video": i.Video,
		"tools": i.Tools, "sampling": i.Sampling, "reasoning": i.Reasoning,
	} {
		if !isNilValue(control) {
			if err := control.Validate(); err != nil {
				return fmt.Errorf("%s intent: %w", name, err)
			}
		}
	}
	if i.Text == nil && i.Image == nil && i.Audio == nil && i.Video == nil {
		if i.Tools.Choice == nil ||
			(i.Tools.Choice.Kind != ToolChoiceRequired &&
				i.Tools.Choice.Kind != ToolChoiceNamed) {
			return fmt.Errorf("tools-only intent requires required or named tool choice")
		}
	}
	return nil
}

type TextIntent struct {
	Response        *ResponseFormat `json:"response,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
}

func (i TextIntent) Clone() TextIntent {
	if i.Response != nil {
		response := *i.Response
		response.Schema = json.RawMessage(append([]byte(nil), response.Schema...))
		i.Response = &response
	}
	i.MaxOutputTokens = clonePointer(i.MaxOutputTokens)
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
	Voice  media.VoiceSpec   `json:"voice"`
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
	if err := i.Voice.Validate(); err != nil {
		return err
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
// completed response carries at least one VideoPart.
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

type ToolsIntent struct {
	Definitions []tool.Definition `json:"definitions,omitempty"`
	Choice      *ToolChoice       `json:"choice,omitempty"`
}

func (i ToolsIntent) Clone() ToolsIntent {
	definitions := make([]tool.Definition, len(i.Definitions))
	for index, definition := range i.Definitions {
		definitions[index] = definition.Clone()
	}
	i.Definitions = definitions
	i.Choice = clonePointer(i.Choice)
	return i
}

func (i ToolsIntent) Validate() error {
	names := make(map[string]struct{}, len(i.Definitions))
	for _, definition := range i.Definitions {
		if err := definition.Validate(); err != nil {
			return err
		}
		if _, exists := names[definition.Name]; exists {
			return fmt.Errorf("duplicate tool definition %q", definition.Name)
		}
		names[definition.Name] = struct{}{}
	}
	if i.Choice == nil {
		if len(i.Definitions) == 0 {
			return fmt.Errorf("tools intent requires definitions or a choice")
		}
		return nil
	}
	if err := i.Choice.Validate(); err != nil {
		return err
	}
	if len(i.Definitions) == 0 && i.Choice.Kind != ToolChoiceNone {
		return fmt.Errorf("tool choice requires tool definitions")
	}
	if i.Choice.Kind == ToolChoiceNamed {
		if _, exists := names[i.Choice.Name]; !exists {
			return fmt.Errorf("named tool choice %q is not defined", i.Choice.Name)
		}
	}
	return nil
}

type SamplingIntent struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
}

func (i SamplingIntent) Clone() SamplingIntent {
	i.Temperature = clonePointer(i.Temperature)
	i.TopP = clonePointer(i.TopP)
	return i
}

func (i SamplingIntent) Validate() error {
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
	return nil
}

type ReasoningIntent struct {
	Effort ReasoningEffort `json:"effort,omitempty"`
}

func (i ReasoningIntent) Validate() error {
	switch i.Effort {
	case "", ReasoningLow, ReasoningMedium, ReasoningHigh:
		return nil
	default:
		return fmt.Errorf("unknown reasoning effort %q", i.Effort)
	}
}

type GenerateResponse struct {
	Message      Message      `json:"message"`
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage"`
	Metadata     Metadata     `json:"metadata"`
}

func (r GenerateResponse) Clone() GenerateResponse {
	r.Message = r.Message.Clone()
	r.Usage = r.Usage.Clone()
	r.Metadata.Decisions = append([]Decision(nil), r.Metadata.Decisions...)
	return r
}

func (r GenerateResponse) Validate() error {
	if r.Message.Role != RoleAssistant {
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
		switch part.(type) {
		case TextPart, ImagePart, AudioPart, VideoPart, ToolCallPart:
		default:
			return fmt.Errorf("generate response contains unsupported part %q", part.Kind())
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
	var text strings.Builder
	textParts := 0
	var images []ImagePart
	var audio []AudioPart
	var videos []VideoPart
	var toolCalls []ToolCallPart
	for _, part := range r.Message.Content.Parts {
		switch value := part.(type) {
		case TextPart:
			if intent.Text == nil {
				return fmt.Errorf("generate response contains unrequested text")
			}
			textParts++
			text.WriteString(value.Text)
		case ImagePart:
			if intent.Image == nil {
				return fmt.Errorf("generate response contains unrequested image")
			}
			images = append(images, value)
			if err := validateGenerateImage(value, *intent.Image); err != nil {
				return fmt.Errorf("generate image %d: %w", len(images)-1, err)
			}
		case AudioPart:
			if intent.Audio == nil {
				return fmt.Errorf("generate response contains unrequested audio")
			}
			audio = append(audio, value)
			if err := validateGenerateAudio(value, *intent.Audio); err != nil {
				return fmt.Errorf("generate audio %d: %w", len(audio)-1, err)
			}
		case VideoPart:
			if intent.Video == nil {
				return fmt.Errorf("generate response contains unrequested video")
			}
			videos = append(videos, value)
			if err := validateGenerateVideo(value); err != nil {
				return fmt.Errorf("generate video %d: %w", len(videos)-1, err)
			}
		case ToolCallPart:
			if intent.Tools == nil {
				return fmt.Errorf("generate response contains an unrequested tool call")
			}
			toolCalls = append(toolCalls, value)
		}
	}
	if intent.Tools != nil {
		definitions := make(map[string]struct{}, len(intent.Tools.Definitions))
		for _, definition := range intent.Tools.Definitions {
			definitions[definition.Name] = struct{}{}
		}
		for index, call := range toolCalls {
			if _, ok := definitions[call.Call.Name]; !ok {
				return fmt.Errorf("generate tool call %d names undefined tool %q", index, call.Call.Name)
			}
		}
		if intent.Tools.Choice != nil {
			switch intent.Tools.Choice.Kind {
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
					if call.Call.Name != intent.Tools.Choice.Name {
						return fmt.Errorf(
							"named tool choice %q produced tool %q",
							intent.Tools.Choice.Name,
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
func validateGenerateVideo(part VideoPart) error {
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
		switch value := part.(type) {
		case ImagePart:
			hasImage = true
			imageCount++
		case AudioPart:
			hasAudio = true
			if value.DurationMillis == nil {
				audioDurationKnown = false
			} else {
				audioDuration += *value.DurationMillis
			}
		case VideoPart:
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

func validateGenerateImage(part ImagePart, intent ImageIntent) error {
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

func validateGenerateAudio(part AudioPart, intent AudioIntent) error {
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
