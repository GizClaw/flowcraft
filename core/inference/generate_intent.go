package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

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

// OutputKinds returns the output content kinds this intent requests, in
// canonical part order: text, image, audio, video. It is the single mapping
// from the intent structure to the message content vocabulary; response
// validation and capability-aware routing share it.
func (i Intent) OutputKinds() []message.PartKind {
	kinds := make([]message.PartKind, 0, 4)
	if i.Text != nil {
		kinds = append(kinds, message.PartText)
	}
	if i.Image != nil {
		kinds = append(kinds, message.PartImage)
	}
	if i.Audio != nil {
		kinds = append(kinds, message.PartAudio)
	}
	if i.Video != nil {
		kinds = append(kinds, message.PartVideo)
	}
	return kinds
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
	Tools      []message.ToolDefinition `json:"tools,omitempty"`
	ToolChoice *ToolChoice              `json:"tool_choice,omitempty"`
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
		tools := make([]message.ToolDefinition, len(i.Tools))
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
	Size         *media.ImageSize   `json:"size,omitempty"`
	AspectRatio  media.AspectRatio  `json:"aspect_ratio,omitempty"`
	Count        *int               `json:"count,omitempty"`
	Seed         *int64             `json:"seed,omitempty"`
	OutputFormat media.ImageFormat  `json:"output_format,omitempty"`
	Delivery     media.SourceKind   `json:"delivery,omitempty"`
	Quality      media.ImageQuality `json:"quality,omitempty"`
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
	if i.Quality != "" {
		if err := i.Quality.Validate(); err != nil {
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

// ReasoningEffort is the request-side "how hard should the model think"
// knob. It is a wire-level string enum, but it is an inference
// concept (not a message part, not a tool DTO) so it lives here
// rather than in [github.com/GizClaw/flowcraft/core/message].
type ReasoningEffort string

const (
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

// FinishReason tells the caller why a generate call stopped. It is
// part of the inference response envelope, not a property of a
// [github.com/GizClaw/flowcraft/core/message.Message], so it lives
// here next to the rest of the inference control types.
type FinishReason string

const (
	FinishCompleted       FinishReason = "completed"
	FinishMaxOutput       FinishReason = "max_output"
	FinishToolCalls       FinishReason = "tool_calls"
	FinishContentFilter   FinishReason = "content_filter"
	FinishRefusal         FinishReason = "refusal"
	FinishPause           FinishReason = "pause"
	FinishInvalidToolCall FinishReason = "invalid_tool_call"
	FinishContextLimit    FinishReason = "context_limit"
	FinishOther           FinishReason = "other"
)

func (r FinishReason) Validate() error {
	switch r {
	case FinishCompleted, FinishMaxOutput, FinishToolCalls, FinishContentFilter,
		FinishRefusal, FinishPause, FinishInvalidToolCall, FinishContextLimit,
		FinishOther:
		return nil
	default:
		return fmt.Errorf("unknown generate finish reason %q", r)
	}
}

// ToolChoiceKind enumerates the request-side strategies for picking
// a tool from the declared [github.com/GizClaw/flowcraft/core/message.ToolDefinition]s.
type ToolChoiceKind string

const (
	ToolChoiceAuto     ToolChoiceKind = "auto"
	ToolChoiceNone     ToolChoiceKind = "none"
	ToolChoiceRequired ToolChoiceKind = "required"
	ToolChoiceNamed    ToolChoiceKind = "named"
)

// ToolChoice is the inference-side "how should the model pick a
// tool" instruction. The set of available tools still rides in
// TextIntent.Tools as a slice of [github.com/GizClaw/flowcraft/core/message.ToolDefinition];
// ToolChoice only decides the selection rule.
type ToolChoice struct {
	Kind ToolChoiceKind `json:"kind"`
	Name string         `json:"name,omitempty"`
}

func (c ToolChoice) Validate() error {
	switch c.Kind {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		if c.Name != "" {
			return fmt.Errorf("tool choice %q cannot name a tool", c.Kind)
		}
	case ToolChoiceNamed:
		if c.Name == "" {
			return fmt.Errorf("named tool choice requires a name")
		}
	default:
		return fmt.Errorf("unknown tool choice %q", c.Kind)
	}
	return nil
}

// ResponseFormatKind enumerates the response-shape strategies a
// caller can request.
type ResponseFormatKind string

const (
	ResponseText       ResponseFormatKind = "text"
	ResponseJSONObject ResponseFormatKind = "json_object"
	ResponseJSONSchema ResponseFormatKind = "json_schema"
)

// ResponseFormat is the request-side "shape the response should
// take" knob. Providers that honor it constrain their output to
// the declared shape; providers that don't reject the request.
type ResponseFormat struct {
	Kind   ResponseFormatKind `json:"kind"`
	Name   string             `json:"name,omitempty"`
	Schema json.RawMessage    `json:"schema,omitempty"`
}

func (f ResponseFormat) Validate() error {
	switch f.Kind {
	case ResponseText, ResponseJSONObject:
		if len(f.Schema) != 0 || f.Name != "" {
			return fmt.Errorf("response format %q cannot carry a schema", f.Kind)
		}
	case ResponseJSONSchema:
		schema := bytes.TrimSpace(f.Schema)
		if f.Name == "" || len(schema) == 0 || schema[0] != '{' || !json.Valid(schema) {
			return fmt.Errorf("JSON schema response requires a name and valid schema")
		}
		compiler := newInMemoryJSONSchemaCompiler()
		const resource = "inference://generate-request-schema.json"
		if err := compiler.AddResource(resource, bytes.NewReader(schema)); err != nil {
			return fmt.Errorf("load response JSON schema: %w", err)
		}
		if _, err := compiler.Compile(resource); err != nil {
			return fmt.Errorf("compile response JSON schema: %w", err)
		}
	default:
		return fmt.Errorf("unknown response format %q", f.Kind)
	}
	return nil
}

// newInMemoryJSONSchemaCompiler builds a [jsonschema.Compiler] that
// refuses to load anything over the network. It exists to validate
// user-supplied [ResponseFormat] schemas at request time without
// letting a malicious schema drag in remote resources.
func newInMemoryJSONSchemaCompiler() *jsonschema.Compiler {
	compiler := jsonschema.NewCompiler()
	compiler.LoadURL = func(resource string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("external JSON schema resource %q is not allowed", resource)
	}
	return compiler
}
