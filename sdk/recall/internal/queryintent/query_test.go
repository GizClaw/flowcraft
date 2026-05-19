package queryintent

import (
	"context"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

func TestRuleBased_ExtractsCapitalizedEntities(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), Input{
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
	out, err := RuleBased{}.Compile(context.Background(), Input{
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
	out, err := RuleBased{}.Compile(context.Background(), Input{
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
	out, err := RuleBased{}.Compile(context.Background(), Input{
		Text: "When did Caroline go to the LGBTQ support group?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != "" {
		t.Fatalf("temporal question should not infer broad subject-only structured lookup, got %q", out.Subject)
	}
	wantKinds := []model.FactKind{model.KindEvent, model.KindState, model.KindPlan}
	if !slices.Equal(out.Kinds, wantKinds) {
		t.Fatalf("kinds = %v, want %v", out.Kinds, wantKinds)
	}
}

func TestRuleBased_InferProfileRelationSubject(t *testing.T) {
	out, err := RuleBased{}.Compile(context.Background(), Input{
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
