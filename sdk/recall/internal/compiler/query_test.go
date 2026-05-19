package compiler

import (
	"context"
	"slices"
	"testing"
)

func TestRuleBasedQueryCompiler_ExtractsCapitalizedEntities(t *testing.T) {
	out, err := RuleBasedQueryCompiler{}.Compile(context.Background(), QueryInput{
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

func TestRuleBasedQueryCompiler_MergesExplicitEntities(t *testing.T) {
	out, err := RuleBasedQueryCompiler{}.Compile(context.Background(), QueryInput{
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

func TestRuleBasedQueryCompiler_PreservesStructuredHints(t *testing.T) {
	out, err := RuleBasedQueryCompiler{}.Compile(context.Background(), QueryInput{
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

func TestExtractEntitiesFromText_SkipsStopwords(t *testing.T) {
	got := extractEntitiesFromText("What is the capital of France?")
	if slices.Contains(got, "what") || slices.Contains(got, "the") {
		t.Fatalf("stopwords leaked: %v", got)
	}
	if !slices.Contains(got, "france") {
		t.Fatalf("want france in %v", got)
	}
}
