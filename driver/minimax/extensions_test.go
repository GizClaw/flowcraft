package minimax

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// mmPtr is the pointer helper these tables read better with. `internal/ptr`
// clones a pointer but cannot make one, so a literal needs this.
func mmPtr[T any](value T) *T { return &value }

// Clone is what the runtime hands a second caller. A shared pointer would let one
// conversation's speech settings change another's, so the clone must own its
// leaves.
//
// The validation tables live in conformance_test.go, which already covers the
// TTS and video rules.
func TestExtensionCloneIsDeep(t *testing.T) {
	tts := TTSOptions{Speed: mmPtr(1.25)}
	clonedTTS := tts.Clone().(*TTSOptions)
	*clonedTTS.Speed = 0.5
	if *tts.Speed != 1.25 {
		t.Errorf("speed mutated through the clone: %v", *tts.Speed)
	}

	video := VideoOptions{
		DurationMillis: mmPtr(int64(6000)),
		Watermark:      mmPtr(false),
		LastFrameOnly:  mmPtr(true),
	}
	clonedVideo := video.Clone().(*VideoOptions)
	*clonedVideo.DurationMillis = 1
	*clonedVideo.Watermark = true
	*clonedVideo.LastFrameOnly = false
	if *video.DurationMillis != 6000 || *video.Watermark || !*video.LastFrameOnly {
		t.Errorf("video options mutated through the clone: %+v", video)
	}
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
