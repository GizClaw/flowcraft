package minimax

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
)

// mmPtr is the pointer helper these tables read better with. `internal/ptr`
// clones a pointer but cannot make one, so a literal needs this.
func mmPtr[T any](value T) *T { return &value }

// The contract every extension owes — an addressable identity, an ActiveFields
// that reports exactly the options it sets, a Validate that accepts its own
// sample, and a Clone that owns its storage — is verified by the shared suite
// rather than re-derived here.
//
// The validation tables live in conformance_test.go, which already covers the TTS
// and video rules.
func TestExtensionContract(t *testing.T) {
	tts := TTSOptions{Speed: mmPtr(1.25), Voice: "Cherry", Language: "zh"}
	video := VideoOptions{
		DurationMillis: mmPtr(int64(6000)),
		Watermark:      mmPtr(false),
		LastFrameOnly:  mmPtr(true),
	}
	inferencetest.RunExtensions(t,
		inferencetest.ExtensionSample{
			Name:   tts.ExtensionID(),
			Value:  &tts,
			Fields: []inference.ExtensionField{"speed", "voice", "language"},
		},
		inferencetest.ExtensionSample{
			Name:   video.ExtensionID(),
			Value:  &video,
			Fields: []inference.ExtensionField{"duration_millis", "watermark", "last_frame_only"},
		},
	)
}

// The runtime clones an extension set before it builds a request. A clone has to
// stay the type this driver asserts on, or a speech deployment's voice stops being
// understood the moment the framework copies it.
func TestAClonedExtensionIsStillUnderstood(t *testing.T) {
	cloned := inference.Extensions{&TTSOptions{Voice: "Cherry"}}.Clone()
	if _, err := ttsOptions(cloned); err != nil {
		t.Fatalf("a cloned extension was not understood: %v", err)
	}
}
