package runners

import (
	"strings"
	"testing"
)

func TestRenderRawTurnContentIncludesStructuredMetadata(t *testing.T) {
	got := RenderRawTurnContent(RawTurn{
		Content:   "Take a look at this.",
		Speaker:   "Alice",
		Timestamp: "9:00 am on 7 May, 2024",
		Images: []RawImage{{
			URL:     "https://example/image.jpg",
			Query:   "ceramic bowl",
			Caption: "a photo of a bowl on a table",
		}},
	})
	for _, want := range []string{
		"[9:00 am on 7 May, 2024] Alice: Take a look at this.",
		"speaker_shared_image (image shared by the speaker in this turn; metadata is not quoted speech):",
		"query: ceramic bowl",
		"caption: a photo of a bowl on a table",
		"url: https://example/image.jpg",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered turn missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ATTACHED_IMAGE_METADATA") {
		t.Fatalf("rendered turn should not use legacy LoCoMo image marker:\n%s", got)
	}
}
