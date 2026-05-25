package intent

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestRuleBased_ExtractsCapitalizedEntities(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), port.IntentInput{
		Text: "Who did Alice meet in Paris?",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alice", "paris"}
	if !slices.Equal(out.Entities, want) {
		t.Fatalf("entities = %v, want %v", out.Entities, want)
	}
}

func TestRuleBased_MergesExplicitEntities(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), port.IntentInput{
		Text:     "When did they travel?",
		Entities: []string{"Bob", "ALICE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Entities, []string{"bob", "alice"}) {
		t.Fatalf("entities = %v", out.Entities)
	}
}

func TestRuleBased_PreservesStructuredHints(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), port.IntentInput{
		Text:      "hello",
		Subject:   "alice",
		Predicate: "city",
		Object:    "paris",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != "alice" || out.Predicate != "city" || out.Object != "paris" {
		t.Fatalf("structured hints not preserved: %+v", out)
	}
}

func TestRuleBased_InferTemporalIntent(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), port.IntentInput{
		Text: "When did Caroline go to the LGBTQ support group?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != "" {
		t.Fatalf("temporal question should not infer broad subject-only structured lookup, got %q", out.Subject)
	}
	wantKinds := []domain.FactKind{domain.KindEvent, domain.KindState, domain.KindPlan}
	if !slices.Equal(out.Kinds, wantKinds) {
		t.Fatalf("kinds = %v, want %v", out.Kinds, wantKinds)
	}
	if !out.Features.Temporal.HasIntent || !out.Features.HasTimeSignal() {
		t.Fatalf("features should carry temporal intent: %+v", out.Features.Temporal)
	}
}

func TestRuleBased_InferDayTimeRangeFromMonthDayYear(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), port.IntentInput{
		Text: "What painting did Melanie show to Caroline on October 13, 2023?",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantFrom := time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2023, time.October, 14, 0, 0, 0, 0, time.UTC)
	if !out.TimeRange.From.Equal(wantFrom) || !out.TimeRange.To.Equal(wantTo) {
		t.Fatalf("time range = %s..%s, want %s..%s", out.TimeRange.From, out.TimeRange.To, wantFrom, wantTo)
	}
	if !out.Features.Temporal.HasExplicitDate {
		t.Fatalf("features should carry explicit date: %+v", out.Features.Temporal)
	}
}

func TestRuleBased_InferMonthTimeRangeFromMonthYear(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), port.IntentInput{
		Text: "Where did Joanna travel to in July 2022?",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantFrom := time.Date(2022, time.July, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2022, time.August, 1, 0, 0, 0, 0, time.UTC)
	if !out.TimeRange.From.Equal(wantFrom) || !out.TimeRange.To.Equal(wantTo) {
		t.Fatalf("time range = %s..%s, want %s..%s", out.TimeRange.From, out.TimeRange.To, wantFrom, wantTo)
	}
}

func TestRuleBased_PreservesExplicitTimeRange(t *testing.T) {
	explicit := domain.TimeRange{
		From: time.Date(2024, time.May, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2024, time.May, 2, 0, 0, 0, 0, time.UTC),
	}
	out, err := RuleBased{}.Compile(context.Background(), port.IntentInput{
		Text:      "What happened on October 13, 2023?",
		TimeRange: explicit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimeRange.From.Equal(explicit.From) || !out.TimeRange.To.Equal(explicit.To) {
		t.Fatalf("time range = %+v, want explicit %+v", out.TimeRange, explicit)
	}
	if out.Features.Temporal.TimeRange.IsZero() {
		t.Fatal("features should still describe query calendar expression")
	}
}

func TestRuleBased_InferProfileRelationSubject(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), port.IntentInput{
		Text: "What fields would Caroline be likely to pursue in her education?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != "caroline" {
		t.Fatalf("subject = %q, want caroline", out.Subject)
	}
}

func TestExtractEntitiesFromText_SkipsStopwords(t *testing.T) {
	got := extractEntitiesFromText("What is the capital of France?")
	if slices.Contains(got, "what") || slices.Contains(got, "the") {
		t.Fatalf("stopwords leaked: %v", got)
	}
	if !slices.Contains(got, "france") {
		t.Fatalf("want france in %v", got)
	}
}
