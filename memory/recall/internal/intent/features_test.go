package intent

import (
	"testing"
	"time"
)

func TestExtractFeaturesDoesNotInferTemporalFromQuestionCue(t *testing.T) {
	features := ExtractFeatures("When did Avery go to the community meetup?")
	if features.HasTimeSignal() {
		t.Fatalf("question cue should not become structural temporal intent, got %+v", features.Temporal)
	}
	if features.Temporal.TimeRange.IsZero() == false {
		t.Fatalf("question without explicit date should not infer range: %+v", features.Temporal.TimeRange)
	}
}

func TestExtractFeaturesAvoidsProceduralBeforeAsTemporalIntent(t *testing.T) {
	features := ExtractFeatures("Before processing invoices, run OCR and then extract entities.")
	if features.HasTimeSignal() {
		t.Fatalf("procedural before should not be temporal literal signal: %+v", features.Temporal)
	}
}

func TestExtractFeaturesTimeRangeFromCalendar(t *testing.T) {
	features := ExtractFeatures("What painting did Jordan show on October 13, 2023?")
	wantFrom := time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2023, time.October, 14, 0, 0, 0, 0, time.UTC)
	if !features.Temporal.HasExplicitDate {
		t.Fatalf("expected explicit date: %+v", features.Temporal)
	}
	if !features.Temporal.TimeRange.From.Equal(wantFrom) || !features.Temporal.TimeRange.To.Equal(wantTo) {
		t.Fatalf("range = %s..%s, want %s..%s", features.Temporal.TimeRange.From, features.Temporal.TimeRange.To, wantFrom, wantTo)
	}
}

func TestExtractFeaturesMonthRange(t *testing.T) {
	features := ExtractFeatures("Where did Joanna travel to in July 2022?")
	wantFrom := time.Date(2022, time.July, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2022, time.August, 1, 0, 0, 0, 0, time.UTC)
	if !features.Temporal.TimeRange.From.Equal(wantFrom) || !features.Temporal.TimeRange.To.Equal(wantTo) {
		t.Fatalf("range = %s..%s, want %s..%s", features.Temporal.TimeRange.From, features.Temporal.TimeRange.To, wantFrom, wantTo)
	}
}

func TestExtractFeaturesUsesNaturalTimexParser(t *testing.T) {
	features := ExtractFeaturesAt("What did Alice say next Tuesday?", time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
	if !features.Temporal.HasRelativeExpression || !features.HasTimeSignal() {
		t.Fatalf("expected natural relative timex: %+v", features.Temporal)
	}
}

func TestExtractFeaturesTimeRangeFromRelativeExpression(t *testing.T) {
	features := ExtractFeaturesAt("What did Alice mention last year?", time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
	wantFrom := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if !features.Temporal.HasRelativeExpression {
		t.Fatalf("expected relative timex: %+v", features.Temporal)
	}
	if !features.Temporal.TimeRange.From.Equal(wantFrom) || !features.Temporal.TimeRange.To.Equal(wantTo) {
		t.Fatalf("range = %s..%s, want %s..%s", features.Temporal.TimeRange.From, features.Temporal.TimeRange.To, wantFrom, wantTo)
	}
}

func TestExtractFeaturesNumericLiteralsOnly(t *testing.T) {
	withLiteral := ExtractFeatures("Did Alice buy 2 pets?")
	if len(withLiteral.Numeric) == 0 {
		t.Fatalf("numeric literal should be preserved: %+v", withLiteral)
	}
}

func TestExtractFeaturesCJKTokenizationWithoutIntentCues(t *testing.T) {
	features := ExtractFeatures("Alice 最早什么时候 moved?")
	if features.HasTimeSignal() {
		t.Fatalf("CJK question cue should not become structural temporal signal: %+v", features.Temporal)
	}
	if len(features.Tokens) == 0 {
		t.Fatalf("CJK tokens should still be preserved: %+v", features)
	}
}

func TestExtractFeaturesDoesNotClassifySemanticQuestionCues(t *testing.T) {
	features := ExtractFeatures("How often did Alice visit after 2021?")
	if len(features.Numeric) == 0 {
		t.Fatalf("numeric literal should still be preserved: %+v", features)
	}
}

func TestHasTimex(t *testing.T) {
	if !HasTimex("I am going tomorrow", time.Date(2024, 5, 7, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("expected rich relative timex")
	}
	if HasTimex("plain account settings", time.Now()) {
		t.Fatal("unexpected timex")
	}
}
