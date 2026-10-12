package dashscope

import (
	"fmt"
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
