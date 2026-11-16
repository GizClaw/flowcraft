package bytedance

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/driver/internal/ptr"
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

const (
	extensionImage = "image_options"
	extensionVideo = "video_options"
)

// providerID is this driver's provider identity: it namespaces every extension
// the package defines, labels the compile errors the ledger builds, and tags
// telemetry. The runtime qualifies extension fields with ProviderID and
// rejects extensions whose provider does not match the resolved model's
// deployment provider, so a deployment that names its provider differently
// must set the Provider field on the options structs it attaches.
const providerID = "bytedance"

// extensionProvider resolves the deployment provider ID an extension targets,
// defaulting to the driver name.
func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return providerID
}

// ---------------------------------------------------------------------------
// Generate (Ark Responses API).
// ---------------------------------------------------------------------------

// GenerateOptions carries Ark Responses API settings that have no canonical
// representation.
type ImageOptions struct {
	// Provider targets a deployment provider ID other than "bytedance".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// Canonical image controls now travel as extensions because the unified
	// request no longer carries a per-modality intent.
	Size         *media.ImageSize   `json:"size,omitempty"`
	Count        *int               `json:"count,omitempty"`
	Seed         *int64             `json:"seed,omitempty"`
	OutputFormat media.ImageFormat  `json:"output_format,omitempty"`
	Delivery     media.SourceKind   `json:"delivery,omitempty"`
	Quality      media.ImageQuality `json:"quality,omitempty"`
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
	// LayerDecomposition asks Seedream 5.0 pro to decompose the input image
	// into one base image and up to 16 independently editable layers.
	// Requires an input image and resolution-level sizing.
	LayerDecomposition *bool `json:"layer_decomposition,omitempty"`
	// Background controls the output alpha channel: "transparent" or
	// "opaque". Seedream 5.0 pro image-to-image only, with exactly one
	// input image that has an alpha channel.
	Background string `json:"background,omitempty"`
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
	if o.Delivery != "" {
		fields = append(fields, "delivery")
	}
	if o.Quality != "" {
		fields = append(fields, "quality")
	}
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
	if o.LayerDecomposition != nil {
		fields = append(fields, "layer_decomposition")
	}
	if o.Background != "" {
		fields = append(fields, "background")
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
	switch o.OutputFormat {
	case "", media.ImageFormatPNG, media.ImageFormatJPEG, media.ImageFormatWebP:
	default:
		return fmt.Errorf("image format %q is not supported", o.OutputFormat)
	}
	switch o.Delivery {
	case "", media.SourceURL, media.SourceInline:
	default:
		return fmt.Errorf("unknown image delivery %q", o.Delivery)
	}
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
	case "", "1k", "1.5k", "2k", "3k", "4k", "adaptive":
	default:
		return fmt.Errorf(
			"size_token must be 1k, 1.5k, 2k, 3k, 4k, or adaptive, not %q",
			o.SizeToken,
		)
	}
	switch o.Background {
	case "", "transparent", "opaque":
	default:
		return fmt.Errorf("background must be transparent or opaque, not %q", o.Background)
	}
	if o.LayerDecomposition != nil && *o.LayerDecomposition && o.Background != "" {
		return fmt.Errorf("background and layer decomposition are mutually exclusive")
	}
	return nil
}

func (o ImageOptions) Clone() inference.Extension {
	o.Size = ptr.Clone(o.Size)
	o.Count = ptr.Clone(o.Count)
	o.Seed = ptr.Clone(o.Seed)
	o.GuidanceScale = ptr.Clone(o.GuidanceScale)
	o.Watermark = ptr.Clone(o.Watermark)
	o.Sequential = ptr.Clone(o.Sequential)
	o.SequentialMaxImages = ptr.Clone(o.SequentialMaxImages)
	o.WebSearch = ptr.Clone(o.WebSearch)
	o.LayerDecomposition = ptr.Clone(o.LayerDecomposition)
	if o.OptimizePrompt != nil {
		optimize := *o.OptimizePrompt
		o.OptimizePrompt = &optimize
	}
	return &o
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
	// Canonical video controls now travel as extensions.
	DurationMillis *int64            `json:"duration_millis,omitempty"`
	Resolution     string            `json:"resolution,omitempty"`
	AspectRatio    media.AspectRatio `json:"aspect_ratio,omitempty"`
	Seed           *int64            `json:"seed,omitempty"`
	Watermark      *bool             `json:"watermark,omitempty"`
	// CameraFixed keeps the camera static while the subject moves.
	CameraFixed *bool `json:"camera_fixed,omitempty"`
	// GenerateAudio lets Seedance 2.x synthesize a matching audio track.
	GenerateAudio *bool `json:"generate_audio,omitempty"`
	// ServiceTier selects the serving tier: "default" or "flex".
	ServiceTier string `json:"service_tier,omitempty"`
	// ExecutionExpiresAfter bounds the server-side task lifetime in seconds.
	ExecutionExpiresAfter *int64 `json:"execution_expires_after,omitempty"`
	// Priority raises the task's queue position; range [0, 9].
	// Seedance 2.5 / 2.0 series only.
	Priority *int32 `json:"priority,omitempty"`
	// OutputFormat selects the container: "mp4" (default) or "mov".
	// Seedance 2.5 only.
	OutputFormat *string `json:"output_format,omitempty"`
	// OmniReferenceTaskType hints the omni-reference subtask:
	// "auto" (default), "reference", "edit", or "extend". Seedance 2.5
	// only; edit/extend impose reference-video, ratio, and duration
	// constraints checked at compile time.
	OmniReferenceTaskType *string `json:"omni_reference_task_type,omitempty"`
	// WebSearch attaches Ark's hosted web_search tool. Seedance 2.5 /
	// 2.0 series only. Search counts are not surfaced: the pinned SDK
	// response Usage has no tool_usage field.
	WebSearch *bool `json:"web_search,omitempty"`
	// CallbackURL receives task status webhook pushes; polling stays the
	// fallback.
	CallbackURL *string `json:"callback_url,omitempty"`
	// SafetyIdentifier is the end-user identifier for abuse detection.
	SafetyIdentifier *string `json:"safety_identifier,omitempty"`
}

func (o VideoOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o VideoOptions) ExtensionID() string { return extensionVideo }

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
	if o.Priority != nil {
		fields = append(fields, "priority")
	}
	if o.OutputFormat != nil {
		fields = append(fields, "output_format")
	}
	if o.OmniReferenceTaskType != nil {
		fields = append(fields, "omni_reference_task_type")
	}
	if o.WebSearch != nil {
		fields = append(fields, "web_search")
	}
	if o.CallbackURL != nil {
		fields = append(fields, "callback_url")
	}
	if o.SafetyIdentifier != nil {
		fields = append(fields, "safety_identifier")
	}
	return fields
}

func (o VideoOptions) Validate() error {
	if o.DurationMillis != nil && *o.DurationMillis <= 0 {
		return fmt.Errorf("duration_millis must be positive")
	}
	if o.Seed != nil && (*o.Seed < -1 || *o.Seed > 2147483647) {
		return fmt.Errorf("seed must be within [-1, 2147483647]")
	}
	switch o.ServiceTier {
	case "", "default", "flex":
	default:
		return fmt.Errorf("service_tier must be default or flex, not %q", o.ServiceTier)
	}
	// Official range: [3600, 259200] seconds (default 172800).
	if o.ExecutionExpiresAfter != nil &&
		(*o.ExecutionExpiresAfter < 3600 || *o.ExecutionExpiresAfter > 259200) {
		return fmt.Errorf(
			"execution_expires_after must be within [3600, 259200], not %d",
			*o.ExecutionExpiresAfter,
		)
	}
	if o.Priority != nil && (*o.Priority < 0 || *o.Priority > 9) {
		return fmt.Errorf("priority must be within [0, 9], not %d", *o.Priority)
	}
	if o.OutputFormat != nil && *o.OutputFormat != "mp4" && *o.OutputFormat != "mov" {
		return fmt.Errorf("output_format must be mp4 or mov, not %q", *o.OutputFormat)
	}
	if o.OmniReferenceTaskType != nil {
		switch *o.OmniReferenceTaskType {
		case "auto", "reference", "edit", "extend":
		default:
			return fmt.Errorf(
				"omni_reference_task_type must be auto, reference, edit, or extend, not %q",
				*o.OmniReferenceTaskType,
			)
		}
	}
	return nil
}

func (o VideoOptions) Clone() inference.Extension {
	o.DurationMillis = ptr.Clone(o.DurationMillis)
	o.Seed = ptr.Clone(o.Seed)
	o.Watermark = ptr.Clone(o.Watermark)
	o.CameraFixed = ptr.Clone(o.CameraFixed)
	o.GenerateAudio = ptr.Clone(o.GenerateAudio)
	o.ExecutionExpiresAfter = ptr.Clone(o.ExecutionExpiresAfter)
	o.Priority = ptr.Clone(o.Priority)
	o.OutputFormat = ptr.Clone(o.OutputFormat)
	o.OmniReferenceTaskType = ptr.Clone(o.OmniReferenceTaskType)
	o.WebSearch = ptr.Clone(o.WebSearch)
	o.CallbackURL = ptr.Clone(o.CallbackURL)
	o.SafetyIdentifier = ptr.Clone(o.SafetyIdentifier)
	return &o
}
