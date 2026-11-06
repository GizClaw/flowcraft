package inference

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/message"
)

// Output names what a call asks the model to produce. It is the only modality
// switch in the contract: the same request type, the same response type and the
// same [Provider] method serve every output, because the answer is always a
// [message.Content] whose parts tell the caller what arrived.
//
// Everything modality-specific that is not the modality itself — an image's
// size and quality, a voice, a duration — is deployment configuration carried by
// the driver's own settings, or a per-call override carried by [Extensions]. It
// never becomes a field on this package's request.
type Output string

const (
	// OutputText asks for the next assistant turn: text, tool calls, or
	// both. It is the default when the field is empty.
	OutputText Output = "text"
	// OutputImage asks for generated images.
	OutputImage Output = "image"
	// OutputAudio asks for synthesized speech.
	OutputAudio Output = "audio"
	// OutputVideo asks for generated video.
	OutputVideo Output = "video"
)

// Validate reports whether the output is one the contract names. The empty
// string is valid: it means [OutputText].
func (o Output) Validate() error {
	switch o {
	case "", OutputText, OutputImage, OutputAudio, OutputVideo:
		return nil
	default:
		return fmt.Errorf("unknown output %q", o)
	}
}

// Kind returns the effective modality, folding the empty default onto
// [OutputText].
func (o Output) Kind() Output {
	if o == "" {
		return OutputText
	}
	return o
}

// PartKind is the content part kind this output produces. It is what a driver
// compares against a model's declared [model.ModelCapabilities].Outputs, and
// what a caller looks for in the response.
func (o Output) PartKind() message.PartKind {
	switch o.Kind() {
	case OutputImage:
		return message.PartImage
	case OutputAudio:
		return message.PartAudio
	case OutputVideo:
		return message.PartVideo
	default:
		return message.PartText
	}
}
