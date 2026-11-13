package minimax

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/driver/internal/ptr"
)

// Provider-specific settings ride on canonical requests as typed extensions
// (inference.Extension). Each surface owns one options struct; the compiler for
// that surface consumes it field by field, and the runtime drops extensions
// addressed to other providers before compiling.
//
// Field names are flat because extension field names may not contain dots.
// providerID is the unexported spelling every error and extension identity in
// this package uses; the exported ProviderID is the deployment-facing one.
const providerID = ProviderID

const (
	extensionTTS   = "tts_options"
	extensionVideo = "video_options"
)

// extensionProvider resolves the deployment provider ID an extension targets,
// defaulting to the driver name.
func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return providerID
}

// TTSOptions carries the per-call speech settings. The API has no canonical
// representation for a voice, so the voice is an extension field and required:
// t2a_v2 refuses a request that names none.
//
// The other fields override the model declaration for one call. A leaf left
// empty keeps the declared value, and the declared value defaults to the
// platform default (mp3).
type TTSOptions struct {
	// Provider targets a deployment provider ID other than "minimax".
	Provider string `json:"-"`
	// Voice is the platform voice id, for example "female-shaonv".
	Voice string `json:"voice,omitempty"`
	// Language steers the spoken language (language_boost). Empty sends
	// "auto", letting the endpoint detect it.
	Language string `json:"language,omitempty"`
	// Speed multiplies the speaking rate; MiniMax serves [0.5, 2].
	Speed *float64 `json:"speed,omitempty"`
	// Format overrides the declared payload encoding. Empty keeps the
	// declaration, and an undeclared encoding is mp3.
	Format media.AudioEncoding `json:"format,omitempty"`
	// SampleRateHz and Channels override the declared stream shape. A raw
	// encoding needs both, whether they come from here or the declaration.
	SampleRateHz int `json:"sample_rate_hz,omitempty"`
	Channels     int `json:"channels,omitempty"`
}

func (o TTSOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o TTSOptions) ExtensionID() string { return extensionTTS }

func (o TTSOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	add := func(name string) { fields = append(fields, inference.ExtensionField(name)) }
	if o.Voice != "" {
		add("voice")
	}
	if o.Language != "" {
		add("language")
	}
	if o.Speed != nil {
		add("speed")
	}
	if o.Format != "" {
		add("format")
	}
	if o.SampleRateHz != 0 {
		add("sample_rate_hz")
	}
	if o.Channels != 0 {
		add("channels")
	}
	return fields
}

func (o TTSOptions) Validate() error {
	if o.Format != "" && !ttsEncodings[o.Format] {
		return fmt.Errorf(
			"format %q is not served; the platform encodes mp3, pcm16, flac, and opus",
			o.Format,
		)
	}
	if o.Speed != nil && (*o.Speed < ttsSpeedMin || *o.Speed > ttsSpeedMax) {
		return fmt.Errorf(
			"speed must be within [%g, %g], not %g",
			ttsSpeedMin, ttsSpeedMax, *o.Speed,
		)
	}
	if o.SampleRateHz != 0 && !ttsSampleRates[o.SampleRateHz] {
		return fmt.Errorf(
			"sample_rate_hz %d is not served; the platform serves 8000/16000/22050/24000/32000/44100",
			o.SampleRateHz,
		)
	}
	if o.Channels != 0 && (o.Channels < 1 || o.Channels > ttsChannelMax) {
		return fmt.Errorf("channels must be 1 or 2, not %d", o.Channels)
	}
	return nil
}

func (o TTSOptions) Clone() inference.Extension {
	o.Speed = ptr.Clone(o.Speed)
	return o
}

// VideoOptions carries the H3 task settings that have no canonical
// representation: the api itself takes a duration, a resolution tier, a ratio,
// a watermark switch, and a callback URL. Every leaf is optional; an unset leaf
// keeps the driver default the platform documents.
type VideoOptions struct {
	// Provider targets a deployment provider ID other than "minimax".
	Provider string `json:"-"`
	// DurationMillis is the clip length. The v2 API serves whole seconds
	// within [4, 15]; unset defaults to 6s.
	DurationMillis *int64 `json:"duration_millis,omitempty"`
	// Resolution is the output tier: 768P or 2K. Unset defaults to 768P.
	Resolution string `json:"resolution,omitempty"`
	// AspectRatio is the target shape: adaptive, 21:9, 16:9, 4:3, 1:1, 3:4,
	// or 9:16. Text-only tasks must name a non-adaptive ratio and default to
	// 16:9; image and reference tasks default to adaptive. adaptive is not a
	// canonical width:height pair, so it passes through unvalidated here and
	// the surface's own value set decides.
	AspectRatio media.AspectRatio `json:"aspect_ratio,omitempty"`
	// Watermark appends the provider's AIGC watermark when true.
	Watermark *bool `json:"watermark,omitempty"`
	// CallbackURL receives asynchronous task status updates after a challenge
	// handshake; the driver still polls as its primary path.
	CallbackURL string `json:"callback_url,omitempty"`
	// LastFrameOnly marks a single input image as the closing frame
	// (role=last_frame) instead of the opening one — the official
	// last-frame-only image-to-video scenario. It requires exactly one image.
	LastFrameOnly *bool `json:"last_frame_only,omitempty"`
}

func (o VideoOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o VideoOptions) ExtensionID() string { return extensionVideo }

func (o VideoOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	add := func(name string) { fields = append(fields, inference.ExtensionField(name)) }
	if o.DurationMillis != nil {
		add("duration_millis")
	}
	if o.Resolution != "" {
		add("resolution")
	}
	if o.AspectRatio != "" {
		add("aspect_ratio")
	}
	if o.Watermark != nil {
		add("watermark")
	}
	if o.CallbackURL != "" {
		add("callback_url")
	}
	if o.LastFrameOnly != nil {
		add("last_frame_only")
	}
	return fields
}

func (o VideoOptions) Validate() error {
	if o.DurationMillis != nil && *o.DurationMillis <= 0 {
		return fmt.Errorf("duration_millis must be positive")
	}
	if o.Resolution != "" && !videoResolutions[o.Resolution] {
		return fmt.Errorf("resolution must be 768P or 2K, not %q", o.Resolution)
	}
	if o.AspectRatio != "" && !videoRatios[string(o.AspectRatio)] {
		return fmt.Errorf(
			"aspect_ratio must be one of adaptive/21:9/16:9/4:3/1:1/3:4/9:16, not %q",
			o.AspectRatio,
		)
	}
	if o.CallbackURL != "" &&
		!strings.HasPrefix(o.CallbackURL, "https://") &&
		!strings.HasPrefix(o.CallbackURL, "http://") {
		return fmt.Errorf("callback_url must be an http(s) URL")
	}
	return nil
}

func (o VideoOptions) Clone() inference.Extension {
	o.DurationMillis = ptr.Clone(o.DurationMillis)
	o.Watermark = ptr.Clone(o.Watermark)
	o.LastFrameOnly = ptr.Clone(o.LastFrameOnly)
	return o
}

// ttsOptions extracts this driver's speech extension from a request. A second
// one is a duplicate rather than a merge: the runtime keeps the order the caller
// wrote, so the ambiguity is the caller's to resolve.
func ttsOptions(extensions inference.Extensions) (*TTSOptions, error) {
	var found *TTSOptions
	for _, extension := range extensions {
		options, ok := extension.(*TTSOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("minimax: extension %s/%s is not understood",
					extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("minimax: duplicate tts options"))
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

// videoOptions extracts this driver's video extension from a request, with the
// same duplicate rule as the speech surface.
func videoOptions(extensions inference.Extensions) (*VideoOptions, error) {
	var found *VideoOptions
	for _, extension := range extensions {
		options, ok := extension.(*VideoOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("minimax: extension %s/%s is not understood",
					extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("minimax: duplicate video options"))
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
