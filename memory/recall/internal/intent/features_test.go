package intent

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestExtractFeaturesTemporal(t *testing.T) {
	features := ExtractFeatures("When did Caroline go to the LGBTQ support group?")
	if !features.Temporal.HasIntent || !features.HasTimeSignal() {
		t.Fatalf("expected temporal intent, got %+v", features.Temporal)
	}
	if features.Temporal.TimeRange.IsZero() == false {
		t.Fatalf("question without explicit date should not infer range: %+v", features.Temporal.TimeRange)
	}
}

func TestExtractFeaturesAvoidsProceduralBeforeAsTemporalIntent(t *testing.T) {
	features := ExtractFeatures("Before processing invoices, run OCR and then extract entities.")
	if features.Temporal.HasIntent {
		t.Fatalf("procedural before should not be temporal query intent: %+v", features.Temporal)
	}
}

func TestExtractFeaturesTimeRangeFromCalendar(t *testing.T) {
	features := ExtractFeatures("What painting did Melanie show on October 13, 2023?")
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

func TestExtractFeaturesNumericIntentUsesTokenBoundaries(t *testing.T) {
	features := ExtractFeatures("How many pets does Alice have?")
	if !features.NumericIntent {
		t.Fatal("expected numeric intent")
	}
	if !hasNumericKind(features.NumericIntentKind, domain.QueryNumericIntentCount) {
		t.Fatalf("expected count numeric kind, got %v", features.NumericIntentKind)
	}
	if ExtractFeatures("Open the account settings").NumericIntent {
		t.Fatal("account must not match count")
	}
}

func TestExtractFeaturesCJKIntent(t *testing.T) {
	features := ExtractFeatures("Alice 最早什么时候 moved?")
	if !features.Temporal.HasIntent {
		t.Fatalf("expected CJK temporal intent: %+v", features.Temporal)
	}
	if !ExtractFeatures("Alice 有多少只猫?").NumericIntent {
		t.Fatal("expected CJK numeric intent")
	}
}

func TestExtractFeaturesMultilingualIntentCues(t *testing.T) {
	if !ExtractFeatures("¿Cuántas veces visitó Alice?").NumericIntent {
		t.Fatal("expected Spanish numeric intent")
	}
	if !ExtractFeatures("Depuis quand Alice habite-t-elle là?").Temporal.HasIntent {
		t.Fatal("expected French temporal intent")
	}
	if !ExtractFeatures("Wie lange dauerte die Reise?").Temporal.HasDurationIntent {
		t.Fatal("expected German duration intent")
	}
	if !ExtractFeatures("Quantas vezes Alice visitou?").NumericIntent {
		t.Fatal("expected Portuguese numeric intent")
	}
}

func TestExtractFeaturesClassifiesIntentKinds(t *testing.T) {
	features := ExtractFeatures("How often did Alice visit after 2021?")
	if !hasNumericKind(features.NumericIntentKind, domain.QueryNumericIntentFrequency) {
		t.Fatalf("expected frequency numeric kind, got %v", features.NumericIntentKind)
	}
	if !hasTemporalKind(features.Temporal.IntentKind, domain.QueryTemporalIntentRange) {
		t.Fatalf("expected temporal range kind, got %v", features.Temporal.IntentKind)
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

func hasNumericKind(kinds []domain.QueryNumericIntentKind, want domain.QueryNumericIntentKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

func hasTemporalKind(kinds []domain.QueryTemporalIntentKind, want domain.QueryTemporalIntentKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}
