package imageutil

import (
	"bytes"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/tool/middleware"
)

// TestDefaultPromptImageBytesFitsThePartBudget pins the prompt-side
// constants against the tool-result budget: an image normalized to
// DefaultPromptImageBytes must marshal into a part no larger than the
// default non-text part budget, so a tool can hand the model a
// normalized image without the limiter dropping it.
func TestDefaultPromptImageBytesFitsThePartBudget(t *testing.T) {
	source, err := media.NewImageBytes(
		bytes.Repeat([]byte{0xAB}, DefaultPromptImageBytes), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := message.MarshalPart(message.ImagePart{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > middleware.DefaultResultPartBudget {
		t.Fatalf(
			"marshalled image part is %d bytes, over the %d-byte part budget",
			len(encoded), middleware.DefaultResultPartBudget)
	}
}
