package dashscope

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/driver/internal/ptr"
	"github.com/goccy/go-json"
)

const (
	extensionGenerate = "generate_options"
	extensionEmbed    = "embed_options"
	providerID        = "dashscope"
)

func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return providerID
}

// GenerateOptions carries the per-call controls the canonical request no
// longer models directly.
type GenerateOptions struct {
	Provider               string          `json:"-"`
	Temperature            *float64        `json:"temperature,omitempty"`
	TopP                   *float64        `json:"top_p,omitempty"`
	MaxOutputTokens        *int            `json:"max_output_tokens,omitempty"`
	Seed                   *int64          `json:"seed,omitempty"`
	TopK                   *int            `json:"top_k,omitempty"`
	RepetitionPenalty      *float64        `json:"repetition_penalty,omitempty"`
	PresencePenalty        *float64        `json:"presence_penalty,omitempty"`
	Stop                   []string        `json:"stop,omitempty"`
	ParallelToolCalls      *bool           `json:"parallel_tool_calls,omitempty"`
	EnableSearch           *bool           `json:"enable_search,omitempty"`
	ThinkingBudget         *int            `json:"thinking_budget,omitempty"`
	VlHighResolutionImages *bool           `json:"vl_high_resolution_images,omitempty"`
	ResponseFormat         *ResponseFormat `json:"response_format,omitempty"`
}

type ResponseFormat struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict bool            `json:"strict,omitempty"`
}

func (o GenerateOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o GenerateOptions) ExtensionID() string { return extensionGenerate }

func (o GenerateOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	add := func(name string) { fields = append(fields, inference.ExtensionField(name)) }
	if o.Temperature != nil {
		add("temperature")
	}
	if o.TopP != nil {
		add("top_p")
	}
	if o.MaxOutputTokens != nil {
		add("max_output_tokens")
	}
	if o.Seed != nil {
		add("seed")
	}
	if o.TopK != nil {
		add("top_k")
	}
	if o.RepetitionPenalty != nil {
		add("repetition_penalty")
	}
	if o.PresencePenalty != nil {
		add("presence_penalty")
	}
	if len(o.Stop) > 0 {
		add("stop")
	}
	if o.ParallelToolCalls != nil {
		add("parallel_tool_calls")
	}
	if o.EnableSearch != nil {
		add("enable_search")
	}
	if o.ThinkingBudget != nil {
		add("thinking_budget")
	}
	if o.VlHighResolutionImages != nil {
		add("vl_high_resolution_images")
	}
	if o.ResponseFormat != nil {
		add("response_format")
	}
	return fields
}

func (o GenerateOptions) Validate() error {
	if o.Temperature != nil && (*o.Temperature < 0 || *o.Temperature > 2) {
		return fmt.Errorf("temperature must be within [0, 2]")
	}
	if o.TopP != nil && (*o.TopP < 0 || *o.TopP > 1) {
		return fmt.Errorf("top_p must be within [0, 1]")
	}
	if o.MaxOutputTokens != nil && *o.MaxOutputTokens <= 0 {
		return fmt.Errorf("max_output_tokens must be positive")
	}
	if o.TopK != nil && *o.TopK <= 0 {
		return fmt.Errorf("top_k must be positive")
	}
	if o.ThinkingBudget != nil && *o.ThinkingBudget < 0 {
		return fmt.Errorf("thinking_budget must not be negative")
	}
	if o.ResponseFormat != nil {
		switch o.ResponseFormat.Kind {
		case "json_object":
		case "json_schema":
			if o.ResponseFormat.Name == "" || len(o.ResponseFormat.Schema) == 0 {
				return fmt.Errorf("json_schema response needs a name and schema")
			}
			if !json.Valid(o.ResponseFormat.Schema) {
				return fmt.Errorf("json_schema response schema is invalid")
			}
		default:
			return fmt.Errorf("unknown response format %q", o.ResponseFormat.Kind)
		}
	}
	return nil
}

func (o GenerateOptions) Clone() inference.Extension {
	o.Temperature = ptr.Clone(o.Temperature)
	o.TopP = ptr.Clone(o.TopP)
	o.MaxOutputTokens = ptr.Clone(o.MaxOutputTokens)
	o.Seed = ptr.Clone(o.Seed)
	o.TopK = ptr.Clone(o.TopK)
	o.RepetitionPenalty = ptr.Clone(o.RepetitionPenalty)
	o.PresencePenalty = ptr.Clone(o.PresencePenalty)
	o.Stop = append([]string(nil), o.Stop...)
	o.ParallelToolCalls = ptr.Clone(o.ParallelToolCalls)
	o.EnableSearch = ptr.Clone(o.EnableSearch)
	o.ThinkingBudget = ptr.Clone(o.ThinkingBudget)
	o.VlHighResolutionImages = ptr.Clone(o.VlHighResolutionImages)
	if o.ResponseFormat != nil {
		format := *o.ResponseFormat
		format.Schema = append(json.RawMessage(nil), o.ResponseFormat.Schema...)
		o.ResponseFormat = &format
	}
	return o
}

// EmbedOptions carries text-embedding controls.
type EmbedOptions struct {
	Provider string `json:"-"`
	TextType string `json:"text_type,omitempty"`
	Instruct string `json:"instruct,omitempty"`
}

func (o EmbedOptions) ProviderID() string         { return extensionProvider(o.Provider) }
func (o EmbedOptions) ExtensionID() string        { return extensionEmbed }
func (o EmbedOptions) Clone() inference.Extension { return o }

func (o EmbedOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.TextType != "" {
		fields = append(fields, "text_type")
	}
	if o.Instruct != "" {
		fields = append(fields, "instruct")
	}
	return fields
}

func (o EmbedOptions) Validate() error {
	switch o.TextType {
	case "", "query", "document":
		return nil
	default:
		return fmt.Errorf("unknown text_type %q", o.TextType)
	}
}

// TTSOptions carries the voice and language the speech surface requires.
type TTSOptions struct {
	Provider string `json:"-"`
	Voice    string `json:"voice,omitempty"`
	Language string `json:"language,omitempty"`
}

func (o TTSOptions) ProviderID() string         { return extensionProvider(o.Provider) }
func (o TTSOptions) ExtensionID() string        { return "tts_options" }
func (o TTSOptions) Clone() inference.Extension { return o }

func (o TTSOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.Voice != "" {
		fields = append(fields, "voice")
	}
	if o.Language != "" {
		fields = append(fields, "language")
	}
	return fields
}

func (o TTSOptions) Validate() error { return nil }

// ImageOptions carries the image controls that used to live on the canonical
// image intent plus the platform's own knobs.
type ImageOptions struct {
	Provider         string            `json:"-"`
	Size             *media.ImageSize  `json:"size,omitempty"`
	Count            *int              `json:"count,omitempty"`
	Seed             *int64            `json:"seed,omitempty"`
	OutputFormat     media.ImageFormat `json:"output_format,omitempty"`
	NegativePrompt   string            `json:"negative_prompt,omitempty"`
	PromptExtend     *bool             `json:"prompt_extend,omitempty"`
	Watermark        *bool             `json:"watermark,omitempty"`
	Style            string            `json:"style,omitempty"`
	RefStrength      *float64          `json:"ref_strength,omitempty"`
	RefMode          string            `json:"ref_mode,omitempty"`
	Function         string            `json:"function,omitempty"`
	IsSketch         *bool             `json:"is_sketch,omitempty"`
	UpscaleFactor    *float64          `json:"upscale_factor,omitempty"`
	EnableInterleave *bool             `json:"enable_interleave,omitempty"`
}

func (o ImageOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o ImageOptions) ExtensionID() string { return "image_options" }
func (o ImageOptions) Clone() inference.Extension {
	o.Size = ptr.Clone(o.Size)
	o.Count = ptr.Clone(o.Count)
	o.Seed = ptr.Clone(o.Seed)
	o.PromptExtend = ptr.Clone(o.PromptExtend)
	o.Watermark = ptr.Clone(o.Watermark)
	o.RefStrength = ptr.Clone(o.RefStrength)
	o.IsSketch = ptr.Clone(o.IsSketch)
	o.UpscaleFactor = ptr.Clone(o.UpscaleFactor)
	o.EnableInterleave = ptr.Clone(o.EnableInterleave)
	return o
}
func (o ImageOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.Size != nil {
		fields = append(fields, "size")
	}
	if o.Count != nil {
		fields = append(fields, "count")
	}
	if o.Seed != nil {
		fields = append(fields, "seed")
	}
	if o.OutputFormat != "" {
		fields = append(fields, "output_format")
	}
	if o.NegativePrompt != "" {
		fields = append(fields, "negative_prompt")
	}
	if o.PromptExtend != nil {
		fields = append(fields, "prompt_extend")
	}
	if o.Watermark != nil {
		fields = append(fields, "watermark")
	}
	if o.Style != "" {
		fields = append(fields, "style")
	}
	if o.RefStrength != nil {
		fields = append(fields, "ref_strength")
	}
	if o.RefMode != "" {
		fields = append(fields, "ref_mode")
	}
	if o.Function != "" {
		fields = append(fields, "function")
	}
	if o.IsSketch != nil {
		fields = append(fields, "is_sketch")
	}
	if o.UpscaleFactor != nil {
		fields = append(fields, "upscale_factor")
	}
	if o.EnableInterleave != nil {
		fields = append(fields, "enable_interleave")
	}
	return fields
}
func (o ImageOptions) Validate() error {
	if o.Size != nil {
		if err := o.Size.Validate(); err != nil {
			return err
		}
	}
	if o.Count != nil && *o.Count <= 0 {
		return fmt.Errorf("count must be positive")
	}
	if o.OutputFormat != "" && o.OutputFormat != media.ImageFormatPNG && o.OutputFormat != media.ImageFormatJPEG && o.OutputFormat != media.ImageFormatWebP {
		return fmt.Errorf("unsupported image format %q", o.OutputFormat)
	}
	if len(o.NegativePrompt) > 500 {
		return fmt.Errorf("negative_prompt must be at most 500 characters")
	}
	if o.RefStrength != nil && (*o.RefStrength < 0 || *o.RefStrength > 1) {
		return fmt.Errorf("ref_strength must be between 0 and 1")
	}
	switch o.RefMode {
	case "", "repaint", "refonly":
	default:
		return fmt.Errorf("unknown ref_mode %q", o.RefMode)
	}
	if o.UpscaleFactor != nil && *o.UpscaleFactor <= 0 {
		return fmt.Errorf("upscale_factor must be positive")
	}
	return nil
}

// VideoOptions carries the video controls that used to live on the canonical
// video intent plus the platform's own knobs.
type VideoOptions struct {
	Provider       string            `json:"-"`
	DurationMillis *int64            `json:"duration_millis,omitempty"`
	Resolution     string            `json:"resolution,omitempty"`
	AspectRatio    media.AspectRatio `json:"aspect_ratio,omitempty"`
	Seed           *int64            `json:"seed,omitempty"`
	Watermark      *bool             `json:"watermark,omitempty"`
	NegativePrompt string            `json:"negative_prompt,omitempty"`
	PromptExtend   *bool             `json:"prompt_extend,omitempty"`
	ShotType       string            `json:"shot_type,omitempty"`
	Audio          *bool             `json:"audio,omitempty"`
	Template       string            `json:"template,omitempty"`
	Link           string            `json:"link,omitempty"`
	SmartDuration  bool              `json:"smart_duration,omitempty"`
}

func (o VideoOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o VideoOptions) ExtensionID() string { return "video_options" }
func (o VideoOptions) Clone() inference.Extension {
	o.DurationMillis = ptr.Clone(o.DurationMillis)
	o.Seed = ptr.Clone(o.Seed)
	o.Watermark = ptr.Clone(o.Watermark)
	o.PromptExtend = ptr.Clone(o.PromptExtend)
	o.Audio = ptr.Clone(o.Audio)
	return o
}
func (o VideoOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.DurationMillis != nil {
		fields = append(fields, "duration_millis")
	}
	if o.Resolution != "" {
		fields = append(fields, "resolution")
	}
	if o.AspectRatio != "" {
		fields = append(fields, "aspect_ratio")
	}
	if o.Seed != nil {
		fields = append(fields, "seed")
	}
	if o.Watermark != nil {
		fields = append(fields, "watermark")
	}
	if o.NegativePrompt != "" {
		fields = append(fields, "negative_prompt")
	}
	if o.PromptExtend != nil {
		fields = append(fields, "prompt_extend")
	}
	if o.ShotType != "" {
		fields = append(fields, "shot_type")
	}
	if o.Audio != nil {
		fields = append(fields, "audio")
	}
	if o.Template != "" {
		fields = append(fields, "template")
	}
	if o.Link != "" {
		fields = append(fields, "link")
	}
	if o.SmartDuration {
		fields = append(fields, "smart_duration")
	}
	return fields
}
func (o VideoOptions) Validate() error {
	if o.DurationMillis != nil && *o.DurationMillis <= 0 {
		return fmt.Errorf("duration_millis must be positive")
	}
	if len(o.NegativePrompt) > 500 {
		return fmt.Errorf("negative_prompt must be at most 500 characters")
	}
	if o.Link != "" && !strings.HasPrefix(o.Link, "http://") && !strings.HasPrefix(o.Link, "https://") {
		return fmt.Errorf("link must be an http(s) URL")
	}
	switch o.ShotType {
	case "", "single", "multi":
	default:
		return fmt.Errorf("unknown shot_type %q", o.ShotType)
	}
	return nil
}

// TranscribeOptions carries the file-transcription controls.
type TranscribeOptions struct {
	Provider          string         `json:"-"`
	ChannelID         *int           `json:"channel_id,omitempty"`
	DisfluencyRemoval *bool          `json:"disfluency_removal_enabled,omitempty"`
	SpecialWordFilter string         `json:"special_word_filter,omitempty"`
	VocabularyID      string         `json:"vocabulary_id,omitempty"`
	Vocabulary        map[string]int `json:"vocabulary,omitempty"`
	KeepDialect       *bool          `json:"keep_dialect,omitempty"`
}

func (o TranscribeOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o TranscribeOptions) ExtensionID() string { return "transcribe_options" }
func (o TranscribeOptions) Clone() inference.Extension {
	o.ChannelID = ptr.Clone(o.ChannelID)
	o.DisfluencyRemoval = ptr.Clone(o.DisfluencyRemoval)
	o.KeepDialect = ptr.Clone(o.KeepDialect)
	if o.Vocabulary != nil {
		m := make(map[string]int, len(o.Vocabulary))
		for k, v := range o.Vocabulary {
			m[k] = v
		}
		o.Vocabulary = m
	}
	return o
}
func (o TranscribeOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.ChannelID != nil {
		fields = append(fields, "channel_id")
	}
	if o.DisfluencyRemoval != nil {
		fields = append(fields, "disfluency_removal_enabled")
	}
	if o.SpecialWordFilter != "" {
		fields = append(fields, "special_word_filter")
	}
	if o.VocabularyID != "" {
		fields = append(fields, "vocabulary_id")
	}
	if len(o.Vocabulary) > 0 {
		fields = append(fields, "vocabulary")
	}
	if o.KeepDialect != nil {
		fields = append(fields, "keep_dialect")
	}
	return fields
}
func (o TranscribeOptions) Validate() error {
	if o.ChannelID != nil && *o.ChannelID < 0 {
		return fmt.Errorf("channel_id must not be negative")
	}
	for term, weight := range o.Vocabulary {
		if (weight < 1 || weight > 5) && weight != 50 {
			return fmt.Errorf("vocabulary weight for %q must be 1-5 or 50", term)
		}
	}
	return nil
}
