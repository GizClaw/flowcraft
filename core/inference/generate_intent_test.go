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

func TestReasoningCapabilityValidate(t *testing.T) {
	kimiMap := map[ReasoningEffort]string{
		ReasoningMinimal: "low",
		ReasoningLow:     "low",
		ReasoningMedium:  "high",
		ReasoningHigh:    "high",
		ReasoningXHigh:   "max",
	}
	if err := (ReasoningCapability{
		Kind:      ReasoningAlways,
		EffortMap: kimiMap,
	}).Validate(); err != nil {
		t.Fatalf("valid capability: %v", err)
	}
	if err := (ReasoningCapability{Kind: ReasoningToggle}).Validate(); err != nil {
		t.Fatalf("binary capability: %v", err)
	}
	for name, capability := range map[string]ReasoningCapability{
		"map without kind": {
			EffortMap: kimiMap,
		},
		"missing canonical key": {
			Kind: ReasoningAlways,
			EffortMap: map[ReasoningEffort]string{
				ReasoningMinimal: "low",
				ReasoningLow:     "low",
				ReasoningMedium:  "high",
				ReasoningHigh:    "high",
			},
		},
		"non-canonical key": {
			Kind: ReasoningAlways,
			EffortMap: map[ReasoningEffort]string{
				ReasoningMinimal: "low",
				ReasoningLow:     "low",
				ReasoningMedium:  "high",
				ReasoningHigh:    "high",
				ReasoningXHigh:   "max",
				"max":            "max",
			},
		},
		"invalid value": {
			Kind: ReasoningAlways,
			EffortMap: map[ReasoningEffort]string{
				ReasoningMinimal: "",
				ReasoningLow:     "low",
				ReasoningMedium:  "high",
				ReasoningHigh:    "high",
				ReasoningXHigh:   "max",
			},
		},
	} {
		if err := capability.Validate(); err == nil {
			t.Fatalf("%s unexpectedly accepted", name)
		}
	}
}

func TestReasoningCapabilityResolveEffort(t *testing.T) {
	capability := ReasoningCapability{
		Kind: ReasoningAlways,
		EffortMap: map[ReasoningEffort]string{
			ReasoningMinimal: "low",
			ReasoningLow:     "low",
			ReasoningMedium:  "high",
			ReasoningHigh:    "high",
			ReasoningXHigh:   "max",
		},
	}
	for _, tc := range []struct {
		effort ReasoningEffort
		want   string
	}{
		{effort: ReasoningMinimal, want: "low"},
		{effort: ReasoningMedium, want: "high"},
		{effort: ReasoningXHigh, want: "max"},
	} {
		mode, ok := capability.ResolveEffort(tc.effort)
		if !ok || mode != tc.want {
			t.Fatalf(
				"resolve(%q) = (%q, %v), want (%q, true)",
				tc.effort, mode, ok, tc.want,
			)
		}
	}
	if _, ok := capability.ResolveEffort("bogus"); ok {
		t.Fatal("non-canonical effort unexpectedly resolved")
	}
}
