package inference

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/message/media"
)

func TestImageIntentQualityValidate(t *testing.T) {
	intent := Intent{
		Image: &ImageIntent{Quality: media.ImageQualityHigh},
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("high quality Validate() = %v, want nil", err)
	}
	intent.Image.Quality = media.ImageQuality("ultra")
	if err := intent.Validate(); err == nil {
		t.Fatal("unknown image quality was accepted")
	}
}

func TestImageIntentCloneCopiesQuality(t *testing.T) {
	intent := Intent{
		Image: &ImageIntent{Quality: media.ImageQualityLow},
	}
	cloned := intent.Clone()
	if cloned.Image.Quality != media.ImageQualityLow {
		t.Fatalf("clone quality = %q, want low", cloned.Image.Quality)
	}
	cloned.Image.Quality = media.ImageQualityHigh
	if intent.Image.Quality != media.ImageQualityLow {
		t.Fatal("clone mutated the source intent")
	}
}
