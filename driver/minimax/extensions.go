package minimax

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Provider-specific settings ride on canonical requests as typed extensions
// (inference.Extension). Each operation family owns one options struct; the
// compiler for that operation consumes it field by field, and any extension
// attached to a request for a different operation is rejected with
// InvalidExtension.
//
// Field names are flat because extension field names may not contain dots.

// driverID namespaces every extension this package defines. The runtime
// qualifies extension fields with ProviderID and rejects extensions whose
// provider does not match the resolved model's deployment provider, so a
// deployment that names its provider differently must set the Provider field
// on the options structs it attaches.
const driverID = "minimax"

const extensionMusic = "music_options"

const extensionVideo = "video_options"

const extensionContextIR = "context_ir_options"

// extensionProvider resolves the deployment provider ID an extension targets,
// defaulting to the driver name.
func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return driverID
}

// MusicOptions carries music_generation settings that have no canonical
// representation: lyrics are a second, structured text input distinct from
// the style prompt, and the instrumental/optimizer switches steer how the
// lyrics are used.
type MusicOptions struct {
	// Provider targets a deployment provider ID other than "minimax".
	// Attempts for any other provider leave the extension inert rather than
	// rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// Lyrics is the song lyrics; lines split on \n and structure tags such
	// as [Verse]/[Chorus] are honored. Optional for instrumental tracks,
	// required otherwise.
	Lyrics string `json:"lyrics,omitempty"`
	// Instrumental requests vocals-free music; lyrics become optional.
	Instrumental *bool `json:"instrumental,omitempty"`
	// LyricsOptimizer auto-generates lyrics from the prompt when Lyrics
	// is empty.
	LyricsOptimizer bool `json:"lyrics_optimizer,omitempty"`
	// Watermark appends an AIGC watermark to the audio; unary requests
	// only.
	Watermark *bool `json:"watermark,omitempty"`
}

func (o MusicOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o MusicOptions) ExtensionID() string { return extensionMusic }

func (o MusicOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.Lyrics != "" {
		fields = append(fields, "lyrics")
	}
	if o.Instrumental != nil {
		fields = append(fields, "instrumental")
	}
	if o.LyricsOptimizer {
		fields = append(fields, "lyrics_optimizer")
	}
	if o.Watermark != nil {
		fields = append(fields, "watermark")
	}
	return fields
}

func (o MusicOptions) Validate() error {
	if len(o.Lyrics) > 3500 {
		return fmt.Errorf("lyrics must be at most 3500 characters")
	}
	return nil
}

func (o MusicOptions) Clone() inference.Extension {
	o.Instrumental = clonePointer(o.Instrumental)
	o.Watermark = clonePointer(o.Watermark)
	return o
}

// VideoOptions carries video task settings that have no canonical
// representation. callback_url applies to both API generations;
// prompt_optimizer and fast_pretreatment are v1-only prompt controls —
// the v2 API has no optimizer knobs, so those fields reject on
// MiniMax-H3.
type VideoOptions struct {
	// Provider targets a deployment provider ID other than "minimax".
	// Attempts for any other provider leave the extension inert rather
	// than rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// CallbackURL receives asynchronous task status updates after a
	// challenge handshake; the driver still polls as its primary path.
	CallbackURL string `json:"callback_url,omitempty"`
	// PromptOptimizer asks the provider to rewrite the prompt before
	// generation (v1 API only; defaults to true server-side).
	PromptOptimizer *bool `json:"prompt_optimizer,omitempty"`
	// FastPretreatment shortens the optimizer's rewrite time (v1 API
	// only; applies to the Hailuo 2.3/2.3-Fast/02 models).
	FastPretreatment *bool `json:"fast_pretreatment,omitempty"`
	// LastFrameOnly marks a single input image as the closing frame
	// (role=last_frame) instead of the opening one. v2 API only; requires
	// exactly one image — the official last-frame-only i2v scenario.
	LastFrameOnly *bool `json:"last_frame_only,omitempty"`
}

func (o VideoOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o VideoOptions) ExtensionID() string { return extensionVideo }

func (o VideoOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.CallbackURL != "" {
		fields = append(fields, "callback_url")
	}
	if o.PromptOptimizer != nil {
		fields = append(fields, "prompt_optimizer")
	}
	if o.FastPretreatment != nil {
		fields = append(fields, "fast_pretreatment")
	}
	if o.LastFrameOnly != nil {
		fields = append(fields, "last_frame_only")
	}
	return fields
}

func (o VideoOptions) Validate() error {
	if o.CallbackURL != "" &&
		!strings.HasPrefix(o.CallbackURL, "https://") &&
		!strings.HasPrefix(o.CallbackURL, "http://") {
		return fmt.Errorf("callback_url must be an http(s) URL")
	}
	return nil
}

func (o VideoOptions) Clone() inference.Extension {
	o.PromptOptimizer = clonePointer(o.PromptOptimizer)
	o.FastPretreatment = clonePointer(o.FastPretreatment)
	o.LastFrameOnly = clonePointer(o.LastFrameOnly)
	return o
}

// ContextIROptions carries H3-Context-IR task settings that have no
// canonical representation. The target video's duration and ratio shape
// the enhanced prompt, and callback_url receives async status updates;
// the content itself rides the request's multimodal parts.
type ContextIROptions struct {
	// Provider targets a deployment provider ID other than "minimax".
	// Attempts for any other provider leave the extension inert rather
	// than rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// DurationMillis is the target video length; defaults to 6s, valid
	// whole seconds within [4, 15].
	DurationMillis *int64 `json:"duration_millis,omitempty"`
	// Ratio is the target video's aspect ratio: adaptive/21:9/16:9/4:3/
	// 1:1/3:4/9:16. Text-only tasks require an explicit non-adaptive
	// ratio (default 16:9); image/reference tasks default to adaptive.
	Ratio string `json:"ratio,omitempty"`
	// CallbackURL receives asynchronous task status updates after a
	// challenge handshake; the driver still polls as its primary path.
	CallbackURL string `json:"callback_url,omitempty"`
}

func (o ContextIROptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o ContextIROptions) ExtensionID() string { return extensionContextIR }

func (o ContextIROptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.DurationMillis != nil {
		fields = append(fields, "duration_millis")
	}
	if o.Ratio != "" {
		fields = append(fields, "ratio")
	}
	if o.CallbackURL != "" {
		fields = append(fields, "callback_url")
	}
	return fields
}

func (o ContextIROptions) Validate() error {
	if o.DurationMillis != nil && *o.DurationMillis <= 0 {
		return fmt.Errorf("duration_millis must be positive")
	}
	if o.Ratio != "" && !v2RatioValues[o.Ratio] {
		return fmt.Errorf(
			"ratio must be one of adaptive/21:9/16:9/4:3/1:1/3:4/9:16, not %q",
			o.Ratio,
		)
	}
	if o.CallbackURL != "" &&
		!strings.HasPrefix(o.CallbackURL, "https://") &&
		!strings.HasPrefix(o.CallbackURL, "http://") {
		return fmt.Errorf("callback_url must be an http(s) URL")
	}
	return nil
}

func (o ContextIROptions) Clone() inference.Extension {
	o.DurationMillis = clonePointer(o.DurationMillis)
	return o
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// operationExtensions splits the request's extensions into the options
// struct this operation consumes and everything else.
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
		if extension == nil {
			continue
		}
		for _, field := range extension.ActiveFields() {
			ledger.reject(
				field.Qualify(extension),
				fmt.Sprintf("%s does not consume %s", operation, extension.ExtensionID()),
			)
		}
	}
}
