package bytedance

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Provider-specific settings ride on canonical requests as typed extensions
// (inference.Extension). Each operation family owns one options struct; the
// compiler for that operation consumes it field by field, and any extension
// attached to a request for a different operation is rejected with
// InvalidExtension. Values are validated twice: Validate runs before compile
// (range and enum checks that need no provider context), and the compiler
// rejects combinations that conflict with the request or the model kind.
//
// Field names are flat because extension field names may not contain dots.

// driverID namespaces every extension this package defines. The runtime
// qualifies extension fields with ProviderID and rejects extensions whose
// provider does not match the resolved model's deployment provider, so a
// deployment that names its provider differently must set the Provider field
// on the options structs it attaches.
const driverID = "bytedance"

const (
	extensionGenerate = "generate_options"
	extensionImage    = "image_options"
	extensionVideo    = "video_options"
	extensionTTS      = "tts_options"
)

// extensionProvider resolves the deployment provider ID an extension targets,
// defaulting to the driver name.
func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return driverID
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// ---------------------------------------------------------------------------
// Generate (Ark Responses API).
// ---------------------------------------------------------------------------

// GenerateOptions carries Ark Responses API settings that have no canonical
// representation.
type GenerateOptions struct {
	// Provider targets a deployment provider ID other than "bytedance".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// ServiceTier selects the serving tier: "auto" or "default".
	ServiceTier string `json:"service_tier,omitempty"`
	// Caching controls context caching for this request.
	Caching *GenerateCaching `json:"caching,omitempty"`
	// Store asks the provider to keep the response server-side.
	Store *bool `json:"store,omitempty"`
	// PreviousResponseID chains this request to a stored response.
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	// ParallelToolCalls allows the model to emit concurrent tool calls.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// MaxToolCalls bounds the number of tool calls in one response.
	MaxToolCalls *int64 `json:"max_tool_calls,omitempty"`
	// WebSearch attaches the provider's web search tool; the model decides
	// when to search.
	WebSearch *GenerateWebSearch `json:"web_search,omitempty"`
}

// GenerateCaching is the caching half of GenerateOptions.
type GenerateCaching struct {
	// Enabled toggles context caching.
	Enabled bool `json:"enabled"`
	// Prefix enables prefix caching; requires Enabled.
	Prefix bool `json:"prefix,omitempty"`
}

// GenerateWebSearch configures the Ark web search tool.
type GenerateWebSearch struct {
	// Limit bounds the number of results per search (provider default 3).
	Limit *int64 `json:"limit,omitempty"`
	// MaxKeyword bounds the keywords per search call.
	MaxKeyword *int32 `json:"max_keyword,omitempty"`
	// Sources restricts content sources: "toutiao", "douyin", "moji",
	// "search_engine". Empty searches every source.
	Sources []string `json:"sources,omitempty"`
	// UserLocation localizes results. Every field is optional; the provider
	// treats the location as approximate.
	UserLocation GenerateWebSearchLocation `json:"user_location,omitempty"`
}

// GenerateWebSearchLocation is the approximate location for web search.
type GenerateWebSearchLocation struct {
	City     string `json:"city,omitempty"`
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

func (o GenerateOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o GenerateOptions) ExtensionID() string { return extensionGenerate }

func (o GenerateOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.ServiceTier != "" {
		fields = append(fields, "service_tier")
	}
	if o.Caching != nil {
		fields = append(fields, "caching_enabled", "caching_prefix")
	}
	if o.Store != nil {
		fields = append(fields, "store")
	}
	if o.PreviousResponseID != "" {
		fields = append(fields, "previous_response_id")
	}
	if o.ParallelToolCalls != nil {
		fields = append(fields, "parallel_tool_calls")
	}
	if o.MaxToolCalls != nil {
		fields = append(fields, "max_tool_calls")
	}
	if o.WebSearch != nil {
		fields = append(fields, "web_search")
	}
	return fields
}

func (o GenerateOptions) Validate() error {
	switch o.ServiceTier {
	case "", "auto", "default":
	default:
		return fmt.Errorf("service_tier must be auto or default, not %q", o.ServiceTier)
	}
	if o.Caching != nil && o.Caching.Prefix && !o.Caching.Enabled {
		return fmt.Errorf("caching prefix requires caching to be enabled")
	}
	if o.MaxToolCalls != nil && *o.MaxToolCalls <= 0 {
		return fmt.Errorf("max_tool_calls must be positive, not %d", *o.MaxToolCalls)
	}
	if search := o.WebSearch; search != nil {
		if search.Limit != nil && *search.Limit <= 0 {
			return fmt.Errorf("web search limit must be positive, not %d", *search.Limit)
		}
		if search.MaxKeyword != nil && *search.MaxKeyword <= 0 {
			return fmt.Errorf("web search max_keyword must be positive, not %d", *search.MaxKeyword)
		}
		for _, source := range search.Sources {
			switch source {
			case "toutiao", "douyin", "moji", "search_engine":
			default:
				return fmt.Errorf("web search source %q is unknown", source)
			}
		}
	}
	return nil
}

func (o GenerateOptions) Clone() inference.Extension {
	o.Store = clonePointer(o.Store)
	o.ParallelToolCalls = clonePointer(o.ParallelToolCalls)
	o.MaxToolCalls = clonePointer(o.MaxToolCalls)
	if o.Caching != nil {
		caching := *o.Caching
		o.Caching = &caching
	}
	if o.WebSearch != nil {
		search := *o.WebSearch
		search.Limit = clonePointer(search.Limit)
		search.MaxKeyword = clonePointer(search.MaxKeyword)
		search.Sources = append([]string(nil), search.Sources...)
		o.WebSearch = &search
	}
	return o
}

// ---------------------------------------------------------------------------
// Image (Seedream images API).
// ---------------------------------------------------------------------------

// ImageOptions carries Seedream images API settings beyond the canonical
// image intent.
type ImageOptions struct {
	// Provider targets a deployment provider ID other than "bytedance".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// GuidanceScale controls prompt adherence; must be positive. Older
	// Seedream generations honor it; newer ones ignore it.
	GuidanceScale *float64 `json:"guidance_scale,omitempty"`
	// Watermark toggles the provider watermark on generated images.
	Watermark *bool `json:"watermark,omitempty"`
	// OptimizePrompt lets the provider rewrite the prompt before generation.
	OptimizePrompt *ImageOptimizePrompt `json:"optimize_prompt,omitempty"`
	// Sequential enables grouped generation ("auto"): the model returns a
	// set of related images in one call. A canonical count intent then maps
	// to the group's max size instead of fanning out repeated calls.
	Sequential *bool `json:"sequential,omitempty"`
	// SequentialMaxImages bounds the group size in [1, 15]; only meaningful
	// with Sequential. Conflicts with a canonical count intent.
	SequentialMaxImages *int `json:"sequential_max_images,omitempty"`
	// SizeToken selects a named size tier ("1k", "2k", "4k") or "adaptive"
	// instead of explicit dimensions. Conflicts with a canonical size.
	SizeToken string `json:"size_token,omitempty"`
	// WebSearch attaches the web search tool (Seedream generations that
	// support tools only).
	WebSearch *bool `json:"web_search,omitempty"`
}

// ImageOptimizePrompt configures provider-side prompt rewriting.
type ImageOptimizePrompt struct {
	// Mode selects "standard" (higher quality, slower) or "fast".
	Mode string `json:"mode,omitempty"`
	// Thinking toggles the optimizer's thinking stage: "auto", "enabled",
	// or "disabled".
	Thinking string `json:"thinking,omitempty"`
}

func (o ImageOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o ImageOptions) ExtensionID() string { return extensionImage }

func (o ImageOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.GuidanceScale != nil {
		fields = append(fields, "guidance_scale")
	}
	if o.Watermark != nil {
		fields = append(fields, "watermark")
	}
	if o.OptimizePrompt != nil {
		fields = append(fields, "optimize_prompt")
	}
	if o.Sequential != nil {
		fields = append(fields, "sequential")
	}
	if o.SequentialMaxImages != nil {
		fields = append(fields, "sequential_max_images")
	}
	if o.SizeToken != "" {
		fields = append(fields, "size_token")
	}
	if o.WebSearch != nil {
		fields = append(fields, "web_search")
	}
	return fields
}

func (o ImageOptions) Validate() error {
	if o.GuidanceScale != nil && *o.GuidanceScale <= 0 {
		return fmt.Errorf("guidance_scale must be positive, not %g", *o.GuidanceScale)
	}
	if optimize := o.OptimizePrompt; optimize != nil {
		switch optimize.Mode {
		case "", "standard", "fast":
		default:
			return fmt.Errorf("optimize_prompt mode must be standard or fast, not %q", optimize.Mode)
		}
		switch optimize.Thinking {
		case "", "auto", "enabled", "disabled":
		default:
			return fmt.Errorf(
				"optimize_prompt thinking must be auto, enabled, or disabled, not %q",
				optimize.Thinking,
			)
		}
	}
	if o.SequentialMaxImages != nil {
		if *o.SequentialMaxImages < 1 || *o.SequentialMaxImages > 15 {
			return fmt.Errorf(
				"sequential_max_images must be in [1, 15], not %d",
				*o.SequentialMaxImages,
			)
		}
		if o.Sequential == nil || !*o.Sequential {
			return fmt.Errorf("sequential_max_images requires sequential generation")
		}
	}
	switch o.SizeToken {
	case "", "1k", "2k", "4k", "adaptive":
	default:
		return fmt.Errorf("size_token must be 1k, 2k, 4k, or adaptive, not %q", o.SizeToken)
	}
	return nil
}

func (o ImageOptions) Clone() inference.Extension {
	o.GuidanceScale = clonePointer(o.GuidanceScale)
	o.Watermark = clonePointer(o.Watermark)
	o.Sequential = clonePointer(o.Sequential)
	o.SequentialMaxImages = clonePointer(o.SequentialMaxImages)
	o.WebSearch = clonePointer(o.WebSearch)
	if o.OptimizePrompt != nil {
		optimize := *o.OptimizePrompt
		o.OptimizePrompt = &optimize
	}
	return o
}

// ---------------------------------------------------------------------------
// Video (Seedance content-generation tasks).
// ---------------------------------------------------------------------------

// VideoOptions carries Seedance task settings beyond the canonical video
// intent. Returning the last frame is deliberately not exposed: the
// canonical contract has no channel for it, and silently dropping a
// requested artifact would be untruthful. Draft-mode task chaining
// (draft / draft_task_id) is likewise out of scope.
type VideoOptions struct {
	// Provider targets a deployment provider ID other than "bytedance".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// CameraFixed keeps the camera static while the subject moves.
	CameraFixed *bool `json:"camera_fixed,omitempty"`
	// GenerateAudio lets Seedance 2.x synthesize a matching audio track.
	GenerateAudio *bool `json:"generate_audio,omitempty"`
	// ServiceTier selects the serving tier: "default" or "flex".
	ServiceTier string `json:"service_tier,omitempty"`
	// ExecutionExpiresAfter bounds the server-side task lifetime in seconds.
	ExecutionExpiresAfter *int64 `json:"execution_expires_after,omitempty"`
}

func (o VideoOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o VideoOptions) ExtensionID() string { return extensionVideo }

func (o VideoOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.CameraFixed != nil {
		fields = append(fields, "camera_fixed")
	}
	if o.GenerateAudio != nil {
		fields = append(fields, "generate_audio")
	}
	if o.ServiceTier != "" {
		fields = append(fields, "service_tier")
	}
	if o.ExecutionExpiresAfter != nil {
		fields = append(fields, "execution_expires_after")
	}
	return fields
}

func (o VideoOptions) Validate() error {
	switch o.ServiceTier {
	case "", "default", "flex":
	default:
		return fmt.Errorf("service_tier must be default or flex, not %q", o.ServiceTier)
	}
	if o.ExecutionExpiresAfter != nil && *o.ExecutionExpiresAfter <= 0 {
		return fmt.Errorf(
			"execution_expires_after must be positive, not %d",
			*o.ExecutionExpiresAfter,
		)
	}
	return nil
}

func (o VideoOptions) Clone() inference.Extension {
	o.CameraFixed = clonePointer(o.CameraFixed)
	o.GenerateAudio = clonePointer(o.GenerateAudio)
	o.ExecutionExpiresAfter = clonePointer(o.ExecutionExpiresAfter)
	return o
}

// ---------------------------------------------------------------------------
// Speech synthesis (Doubao TTS V2).
// ---------------------------------------------------------------------------

// TTSOptions carries Doubao TTS V2 settings beyond the canonical audio
// intent. Rates are percentage offsets: 0 keeps the voice default.
type TTSOptions struct {
	// Provider targets a deployment provider ID other than "bytedance".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// PitchRate adjusts pitch in [-100, 100].
	PitchRate *int `json:"pitch_rate,omitempty"`
	// VolumeRate adjusts loudness in [-100, 100].
	VolumeRate *int `json:"volume_rate,omitempty"`
	// Emotion selects an emotional style supported by the voice.
	Emotion string `json:"emotion,omitempty"`
	// BitRate sets the compressed bitrate; rejected for raw PCM output.
	BitRate *int `json:"bit_rate,omitempty"`
	// Note: the provider's mixed-speaker mode is intentionally absent — the
	// canonical audio intent makes a voice ID mandatory, so a mix replacing
	// the voice has no truthful request shape.
}

func (o TTSOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o TTSOptions) ExtensionID() string { return extensionTTS }

func (o TTSOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.PitchRate != nil {
		fields = append(fields, "pitch_rate")
	}
	if o.VolumeRate != nil {
		fields = append(fields, "volume_rate")
	}
	if o.Emotion != "" {
		fields = append(fields, "emotion")
	}
	if o.BitRate != nil {
		fields = append(fields, "bit_rate")
	}
	return fields
}

func (o TTSOptions) Validate() error {
	for name, rate := range map[string]*int{
		"pitch_rate":  o.PitchRate,
		"volume_rate": o.VolumeRate,
	} {
		if rate != nil && (*rate < -100 || *rate > 100) {
			return fmt.Errorf("%s must be in [-100, 100], not %d", name, *rate)
		}
	}
	if o.BitRate != nil && *o.BitRate <= 0 {
		return fmt.Errorf("bit_rate must be positive, not %d", *o.BitRate)
	}
	return nil
}

func (o TTSOptions) Clone() inference.Extension {
	o.PitchRate = clonePointer(o.PitchRate)
	o.VolumeRate = clonePointer(o.VolumeRate)
	o.BitRate = clonePointer(o.BitRate)
	return o
}

// ---------------------------------------------------------------------------
// Consumption helper.
// ---------------------------------------------------------------------------

// operationExtensions splits request extensions into the options struct
// applying to one operation and every other extension present. The runtime
// has already rejected foreign providers and duplicate identities, so at most
// one T can appear; the caller rejects the remaining extensions' fields.
func operationExtensions[T inference.Extension](
	extensions inference.Extensions,
) (T, []inference.Extension) {
	var options T
	var other []inference.Extension
	for _, extension := range extensions {
		if extension == nil {
			continue
		}
		if typed, ok := extension.(T); ok {
			options = typed
			continue
		}
		other = append(other, extension)
	}
	return options, other
}

// rejectOtherExtensions records a rejection for every active field of
// extensions that do not apply to the operation being compiled.
func rejectOtherExtensions(
	operation string,
	other []inference.Extension,
	ledger *ledger,
) {
	for _, extension := range other {
		reason := fmt.Sprintf(
			"extension %q does not apply to %s",
			extension.ExtensionID(),
			operation,
		)
		for _, field := range extension.ActiveFields() {
			ledger.reject(field.Qualify(extension), reason)
		}
	}
}
