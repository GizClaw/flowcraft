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

func TestTextIntentReasoningEffortValidation(t *testing.T) {
	for _, effort := range []ReasoningEffort{
		"",
		ReasoningMinimal,
		ReasoningLow,
		ReasoningMedium,
		ReasoningHigh,
		ReasoningXHigh,
	} {
		intent := TextIntent{ReasoningEffort: effort}
		if err := intent.Validate(); err != nil {
			t.Fatalf("effort %q: %v", effort, err)
		}
	}
	for _, effort := range []ReasoningEffort{
		"max",
		"bogus",
		" ",
		"low high",
	} {
		intent := TextIntent{ReasoningEffort: effort}
		if err := intent.Validate(); err == nil {
			t.Fatalf("effort %q unexpectedly accepted", effort)
		}
	}
	disabled := false
	intent := TextIntent{
		ReasoningEnabled: &disabled,
		ReasoningEffort:  ReasoningHigh,
	}
	if err := intent.Validate(); err == nil {
		t.Fatal("disabled reasoning with an effort unexpectedly accepted")
	}
}

func TestResolveReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		effort ReasoningEffort
		ladder []ReasoningEffort
		want   ReasoningEffort
		exact  bool
	}{
		{
			name:   "openai ladder is the canonical five",
			effort: ReasoningXHigh,
			ladder: []ReasoningEffort{
				ReasoningMinimal, ReasoningLow, ReasoningMedium,
				ReasoningHigh, ReasoningXHigh,
			},
			want:  ReasoningXHigh,
			exact: true,
		},
		{
			name:   "kimi ladder exact low and high",
			effort: ReasoningLow,
			ladder: []ReasoningEffort{ReasoningLow, ReasoningHigh, "max"},
			want:   ReasoningLow,
			exact:  true,
		},
		{
			name:   "kimi ladder maps minimal to low",
			effort: ReasoningMinimal,
			ladder: []ReasoningEffort{ReasoningLow, ReasoningHigh, "max"},
			want:   ReasoningLow,
		},
		{
			name:   "kimi ladder maps medium to high",
			effort: ReasoningMedium,
			ladder: []ReasoningEffort{ReasoningLow, ReasoningHigh, "max"},
			want:   ReasoningHigh,
		},
		{
			name:   "kimi ladder maps xhigh to max",
			effort: ReasoningXHigh,
			ladder: []ReasoningEffort{ReasoningLow, ReasoningHigh, "max"},
			want:   "max",
		},
		{
			name:   "qwen ladder maps high to xhigh",
			effort: ReasoningHigh,
			ladder: []ReasoningEffort{
				ReasoningLow, ReasoningMedium, ReasoningXHigh,
			},
			want: ReasoningXHigh,
		},
		{
			name:   "qwen ladder maps minimal to low",
			effort: ReasoningMinimal,
			ladder: []ReasoningEffort{
				ReasoningLow, ReasoningMedium, ReasoningXHigh,
			},
			want: ReasoningLow,
		},
		{
			name:   "deepseek ladder drops minimal to low",
			effort: ReasoningMinimal,
			ladder: []ReasoningEffort{
				ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh,
			},
			want: ReasoningLow,
		},
		{
			name:   "anthropic ladder keeps xhigh exact",
			effort: ReasoningXHigh,
			ladder: []ReasoningEffort{
				ReasoningLow, ReasoningMedium, ReasoningHigh,
				ReasoningXHigh, "max",
			},
			want:  ReasoningXHigh,
			exact: true,
		},
		{
			name:   "effort above ladder falls back to the top",
			effort: ReasoningXHigh,
			ladder: []ReasoningEffort{ReasoningLow, ReasoningMedium},
			want:   ReasoningMedium,
		},
		{
			name:   "empty ladder resolves nothing",
			effort: ReasoningHigh,
			want:   "",
		},
	} {
		got, exact := ResolveReasoningEffort(tc.effort, tc.ladder)
		if got != tc.want || exact != tc.exact {
			t.Fatalf(
				"%s: resolve(%q) = (%q, %v), want (%q, %v)",
				tc.name, tc.effort, got, exact, tc.want, tc.exact,
			)
		}
	}
}
