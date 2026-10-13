package dashscope

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// Provider-specific settings ride on canonical requests as typed extensions
// (inference.Extension). Each operation family owns one options struct; the
// compiler for that operation consumes it field by field, and any extension
// attached to a request for a different operation is rejected with
// InvalidExtension.
//
// Field names are flat because extension field names may not contain dots.

// providerID is this driver's provider identity: it namespaces every extension
// the package defines, labels the compile errors the ledger builds, and tags
// telemetry. The runtime qualifies extension fields with ProviderID and
// rejects extensions whose provider does not match the resolved model's
// deployment provider, so a deployment that names its provider differently
// must set the Provider field on the options structs it attaches.
const providerID = "dashscope"

const extensionImage = "image_options"

const extensionVideo = "video_options"

const extensionGenerate = "generate_options"

const extensionEmbed = "embed_options"

const extensionTranscribe = "transcribe_options"

// extensionProvider resolves the deployment provider ID an extension
// targets, defaulting to the driver name.
func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return providerID
}

// ImageOptions carries image settings that have no canonical
// representation: the negative prompt, the platform's prompt-rewrite and
// watermark switches, the wanx-v1 style and reference-image knobs, the
// image-edit function selector, and the interleave switch of the sync
// surface.
type ImageOptions struct {
	// Provider targets a deployment provider ID other than "dashscope".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// NegativePrompt describes what the image should not contain; the
	// platform caps it at 500 characters.
	NegativePrompt string `json:"negative_prompt,omitempty"`
	// PromptExtend toggles the platform's prompt rewrite, which improves
	// short prompts at the cost of a few seconds. Unset leaves the
	// provider's own default (on for most models) in place.
	PromptExtend *bool `json:"prompt_extend,omitempty"`
	// Watermark adds the platform's "AI生成" mark.
	Watermark *bool `json:"watermark,omitempty"`
	// Style selects the wanx-v1 output style, e.g. "<photography>". Only
	// models declaring image.style accept it.
	Style string `json:"style,omitempty"`
	// RefStrength tunes how closely a reference image is followed, in
	// [0, 1]. Only the reference-image surface accepts it.
	RefStrength *float64 `json:"ref_strength,omitempty"`
	// RefMode selects the reference-image mode: "repaint" (content) or
	// "refonly" (style only).
	RefMode string `json:"ref_mode,omitempty"`
	// Function selects the image-edit operation on edit models, e.g.
	// "description_edit_with_mask", "expand", or "super_resolution". Edit
	// models require it.
	Function string `json:"function,omitempty"`
	// IsSketch marks the doodle input as a sketch rather than a
	// photograph to trace; only the doodle function reads it.
	IsSketch *bool `json:"is_sketch,omitempty"`
	// UpscaleFactor is the super-resolution multiplier; only the
	// super_resolution function reads it.
	UpscaleFactor *float64 `json:"upscale_factor,omitempty"`
	// EnableInterleave switches the sync surface into interleaved
	// text-and-image output (wan2.6-image). It is what makes a text intent
	// compilable on an image model.
	EnableInterleave *bool `json:"enable_interleave,omitempty"`
}

func (o ImageOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o ImageOptions) ExtensionID() string { return extensionImage }

// Clone returns a deep copy: the pointer leaves are the caller's values, not
// shared ones.
func (o ImageOptions) Clone() inference.Extension {
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
	if len(o.NegativePrompt) > 500 {
		return fmt.Errorf("negative_prompt must be at most 500 characters")
	}
	if o.RefStrength != nil && (*o.RefStrength < 0 || *o.RefStrength > 1) {
		return fmt.Errorf("ref_strength must be between 0 and 1")
	}
	switch o.RefMode {
	case "", "repaint", "refonly":
	default:
		return fmt.Errorf("unknown ref_mode %q, want repaint or refonly", o.RefMode)
	}
	if o.UpscaleFactor != nil && *o.UpscaleFactor <= 0 {
		return fmt.Errorf("upscale_factor must be positive")
	}
	return nil
}

// VideoOptions carries video settings that have no canonical
// representation: the negative prompt, the prompt-rewrite switch, the
// multi-shot selector of the 2.6-and-later narrative control, the
// output-audio switch, the effect template, and the two knobs only the Wan
// 3.0 media array documents (a web page among the input media, and the
// model-chosen duration).
type VideoOptions struct {
	// Provider targets a deployment provider ID other than "dashscope".
	Provider string `json:"-"`
	// NegativePrompt describes what the video should not contain; the
	// platform caps it at 500 characters.
	NegativePrompt string `json:"negative_prompt,omitempty"`
	// PromptExtend toggles the platform's prompt rewrite. Unset leaves the
	// provider's own default (on for most models) in place.
	PromptExtend *bool `json:"prompt_extend,omitempty"`
	// ShotType selects single or multi shot narration; only models
	// declaring video.shot_type accept it, and it only takes effect while
	// prompt rewriting is on.
	ShotType string `json:"shot_type,omitempty"`
	// Audio switches the output soundtrack on models that can synthesize
	// one; it outranks the input audio when both are present.
	Audio *bool `json:"audio,omitempty"`
	// Template names a video-effect template, which replaces the prompt as
	// the driver of the generation; only models declaring video.template
	// accept it.
	Template string `json:"template,omitempty"`
	// Link names a public web page the model reads as source material,
	// riding input.media as the one link entry; only media declarations
	// naming video.media.link accept it, and it cannot ride a request that
	// already carries a file entry.
	Link string `json:"link,omitempty"`
	// SmartDuration asks the model to pick the length from the prompt and
	// the media (parameters.duration -1) instead of taking a duration from
	// the intent; only models declaring video.smart_duration accept it.
	SmartDuration bool `json:"smart_duration,omitempty"`
}

func (o VideoOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o VideoOptions) ExtensionID() string { return extensionVideo }

// Clone returns a deep copy: the pointer leaves are the caller's values, not
// shared ones.
func (o VideoOptions) Clone() inference.Extension {
	o.PromptExtend = ptr.Clone(o.PromptExtend)
	o.Audio = ptr.Clone(o.Audio)
	return o
}

func (o VideoOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
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
	if len(o.NegativePrompt) > 500 {
		return fmt.Errorf("negative_prompt must be at most 500 characters")
	}
	if o.Link != "" &&
		!strings.HasPrefix(o.Link, "http://") && !strings.HasPrefix(o.Link, "https://") {
		return fmt.Errorf("link must be an http(s) URL")
	}
	switch o.ShotType {
	case "", "single", "multi":
	default:
		return fmt.Errorf("unknown shot_type %q, want single or multi", o.ShotType)
	}
	return nil
}

// GenerateOptions carries text-generation settings that have no canonical
// representation: the sampler's remaining dials, the platform's server-side
// search switches, the thinking budget, and the VL surfaces' high-resolution
// switch.
type GenerateOptions struct {
	// Provider targets a deployment provider ID other than "dashscope".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// Seed pins the sampler where the model supports a deterministic seed.
	Seed *int64 `json:"seed,omitempty"`
	// TopK caps the candidate pool; must be positive.
	TopK *int `json:"top_k,omitempty"`
	// RepetitionPenalty penalizes tokens already present in the output
	// (1.0 is neutral); the platform's own bound decides the range.
	RepetitionPenalty *float64 `json:"repetition_penalty,omitempty"`
	// PresencePenalty shifts the sampler by whether a token already
	// appeared; the platform's own bound decides the range.
	PresencePenalty *float64 `json:"presence_penalty,omitempty"`
	// Stop lists the sequences that end the generation. The platform caps
	// the list at four entries.
	Stop []string `json:"stop,omitempty"`
	// ParallelToolCalls allows several tool calls in one turn. Unset leaves
	// the platform default in place.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// EnableSearch turns the platform's server-side web search on or off.
	// Only models declaring capabilities.hosted_web_search accept it; the
	// retrieved sources come back as the response's SearchOutput.
	EnableSearch *bool `json:"enable_search,omitempty"`
	// SearchOptions tunes the retrieval strategy and the answer
	// annotations. It requires EnableSearch true: search_option only apply
	// while search is enabled.
	SearchOptions *SearchOptions `json:"search_options,omitempty"`
	// ThinkingBudget caps the thinking tokens on models whose reasoning has
	// a depth dial; a request that disables thinking cannot bind a budget.
	ThinkingBudget *int `json:"thinking_budget,omitempty"`
	// VlHighResolutionImages lifts the input-image pixel cap of the VL
	// models to the platform's high-resolution ceiling; only models
	// declaring image input accept it.
	VlHighResolutionImages *bool `json:"vl_high_resolution_images,omitempty"`
}

// SearchOptions is the search_options sub-object of GenerateOptions.
type SearchOptions struct {
	// Strategy selects the retrieval policy: "turbo", "max", or "agent".
	// Which strategies a model serves is the platform's fact — some
	// families reject "agent" — so the driver validates the documented
	// vocabulary and leaves support to the endpoint.
	Strategy string `json:"strategy,omitempty"`
	// EnableSource attaches the source list to the answer.
	EnableSource *bool `json:"enable_source,omitempty"`
	// EnableCitation marks the answer text with citation markers.
	EnableCitation *bool `json:"enable_citation,omitempty"`
	// CitationFormat overrides the citation marker format the platform
	// documents as "[<number>]".
	CitationFormat string `json:"citation_format,omitempty"`
}

func (o GenerateOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o GenerateOptions) ExtensionID() string { return extensionGenerate }

// Clone returns a deep copy: the pointer leaves are the caller's values, not
// shared ones.
func (o GenerateOptions) Clone() inference.Extension {
	o.Seed = ptr.Clone(o.Seed)
	o.TopK = ptr.Clone(o.TopK)
	o.RepetitionPenalty = ptr.Clone(o.RepetitionPenalty)
	o.PresencePenalty = ptr.Clone(o.PresencePenalty)
	o.Stop = append([]string(nil), o.Stop...)
	o.ParallelToolCalls = ptr.Clone(o.ParallelToolCalls)
	o.EnableSearch = ptr.Clone(o.EnableSearch)
	if o.SearchOptions != nil {
		cloned := *o.SearchOptions
		cloned.EnableSource = ptr.Clone(o.SearchOptions.EnableSource)
		cloned.EnableCitation = ptr.Clone(o.SearchOptions.EnableCitation)
		o.SearchOptions = &cloned
	}
	o.ThinkingBudget = ptr.Clone(o.ThinkingBudget)
	o.VlHighResolutionImages = ptr.Clone(o.VlHighResolutionImages)
	return o
}

func (o GenerateOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.Seed != nil {
		fields = append(fields, "seed")
	}
	if o.TopK != nil {
		fields = append(fields, "top_k")
	}
	if o.RepetitionPenalty != nil {
		fields = append(fields, "repetition_penalty")
	}
	if o.PresencePenalty != nil {
		fields = append(fields, "presence_penalty")
	}
	if len(o.Stop) > 0 {
		fields = append(fields, "stop")
	}
	if o.ParallelToolCalls != nil {
		fields = append(fields, "parallel_tool_calls")
	}
	if o.EnableSearch != nil {
		fields = append(fields, "enable_search")
	}
	if o.SearchOptions != nil {
		fields = append(fields, "search_options")
	}
	if o.ThinkingBudget != nil {
		fields = append(fields, "thinking_budget")
	}
	if o.VlHighResolutionImages != nil {
		fields = append(fields, "vl_high_resolution_images")
	}
	return fields
}

func (o GenerateOptions) Validate() error {
	if o.TopK != nil && *o.TopK <= 0 {
		return fmt.Errorf("top_k must be positive")
	}
	if o.ThinkingBudget != nil && *o.ThinkingBudget <= 0 {
		return fmt.Errorf("thinking_budget must be positive")
	}
	for _, stop := range o.Stop {
		if stop == "" {
			return fmt.Errorf("stop sequences must not be empty")
		}
	}
	if o.SearchOptions != nil {
		if err := o.SearchOptions.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the search tuning for structural sanity.
func (o SearchOptions) Validate() error {
	switch o.Strategy {
	case "", "turbo", "max", "agent":
	default:
		return fmt.Errorf(
			"unknown search strategy %q, want turbo, max, or agent",
			o.Strategy,
		)
	}
	if o.CitationFormat != "" && o.EnableCitation != nil && !*o.EnableCitation {
		return fmt.Errorf(
			"citation_format requires citations to be enabled")
	}
	return nil
}

// EmbedOptions carries text-embedding settings that have no canonical
// representation: the query/document role hint of asymmetric retrieval and the
// retrieval instruction the newer models accept.
type EmbedOptions struct {
	// Provider targets a deployment provider ID other than "dashscope".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// TextType marks the input as a retrieval "query" or a stored
	// "document". Unset leaves the platform default (document).
	TextType string `json:"text_type,omitempty"`
	// Instruct adds a retrieval instruction for query embeddings; only
	// deployments declaring embed.instruct accept it, and it binds only
	// with TextType "query".
	Instruct string `json:"instruct,omitempty"`
}

func (o EmbedOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o EmbedOptions) ExtensionID() string { return extensionEmbed }

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
	default:
		return fmt.Errorf(
			"unknown text_type %q, want query or document", o.TextType,
		)
	}
	return nil
}

// TranscribeOptions carries file-transcription settings that have no canonical
// representation: the track selector, the filler-word filter, the
// sensitive-word spec, and the prebuilt and inline hotword channels. Each knob
// is honored only on models whose declaration names it, because the platform
// documents the parameter set per model family.
//
// Speaker diarization is deliberately absent. The platform's
// parameters.diarization_enabled makes the result carry speaker_id on every
// sentence, and the canonical transcription response has no field for a
// speaker: offering the switch would bill the caller for labels this driver
// could not deliver. It joins the extension when the canonical response grows
// a home for speakers.
type TranscribeOptions struct {
	// Provider targets a deployment provider ID other than "dashscope".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// ChannelID selects the audio track to recognize, from 0. Unset leaves
	// the platform default (the first track). Tracks are billed
	// independently, and the canonical response carries one transcript, so
	// the driver sends exactly one index.
	ChannelID *int `json:"channel_id,omitempty"`
	// DisfluencyRemoval filters filler words from the transcript. Unset
	// leaves the provider's own default in place.
	DisfluencyRemoval *bool `json:"disfluency_removal_enabled,omitempty"`
	// SpecialWordFilter carries the sensitive-word handling spec, e.g.
	// "nonsense:1" or "block:bad,awful".
	SpecialWordFilter string `json:"special_word_filter,omitempty"`
	// VocabularyID references a prebuilt hotword list.
	VocabularyID string `json:"vocabulary_id,omitempty"`
	// Vocabulary carries inline hotwords keyed by the term, weighted 1-5 (or
	// 50 for a super hotword).
	Vocabulary map[string]int `json:"vocabulary,omitempty"`
	// KeepDialect keeps dialect wording instead of normalizing it to
	// Mandarin; only qwen-audio-3.1-asr-flash-filetrans reads it.
	KeepDialect *bool `json:"keep_dialect,omitempty"`
}

func (o TranscribeOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o TranscribeOptions) ExtensionID() string { return extensionTranscribe }

// Clone returns a deep copy: the map and the pointer leaves are the caller's
// values, not shared ones.
func (o TranscribeOptions) Clone() inference.Extension {
	o.ChannelID = ptr.Clone(o.ChannelID)
	o.DisfluencyRemoval = ptr.Clone(o.DisfluencyRemoval)
	o.KeepDialect = ptr.Clone(o.KeepDialect)
	o.Vocabulary = maps.Clone(o.Vocabulary)
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
		return fmt.Errorf(
			"channel_id must not be negative, not %d", *o.ChannelID)
	}
	for _, term := range sortedVocabularyKeys(o.Vocabulary) {
		weight := o.Vocabulary[term]
		if (weight < 1 || weight > 5) && weight != 50 {
			return fmt.Errorf(
				"vocabulary weight for %q must be 1-5 or 50, not %d",
				term, weight,
			)
		}
	}
	return nil
}

// sortedVocabularyKeys renders the vocabulary keys in a stable order so a
// validation error names the same term on every run.
func sortedVocabularyKeys(vocabulary map[string]int) []string {
	keys := make([]string, 0, len(vocabulary))
	for term := range vocabulary {
		keys = append(keys, term)
	}
	slices.Sort(keys)
	return keys
}
