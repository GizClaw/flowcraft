package minimax

import (
	"fmt"

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
