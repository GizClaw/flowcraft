package semantic

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
)

func TestProjectionKindMatchesStructuredAssertions(t *testing.T) {
	subjectOnly := domain.TemporalFact{Subject: "Mira"}
	if projectionKindMatches(planner.SourceAssertion, subjectOnly) {
		t.Fatal("assertion projection must not index subject-only facts")
	}

	triple := domain.TemporalFact{Subject: "Mira", Predicate: "visited", Object: "Paris"}
	if !projectionKindMatches(planner.SourceAssertion, triple) {
		t.Fatal("assertion projection should index complete assertion triples")
	}

	contentOnly := domain.TemporalFact{Content: "Mira did not visit Paris."}
	if projectionKindMatches(planner.SourceAssertion, contentOnly) {
		t.Fatal("assertion projection must not use legacy assertion metadata or content-only text as eligibility")
	}
}

func TestSemanticEntryMatchingRequiresMeaningfulOverlap(t *testing.T) {
	entry := semanticEntry{
		text:  "dave prefers dodge charger over subaru forester",
		terms: map[string]struct{}{"dave": {}, "prefer": {}, "dodge": {}, "charger": {}, "subaru": {}, "forester": {}},
	}
	intent := domain.QueryIntent{Text: "Which city did Mira visit?"}
	if entryMatchesIntent(entry, intent, querySemanticTerms(intent)) {
		t.Fatal("bare which selection query must not match unrelated comparison entry")
	}
}

func TestSemanticEntryMatchingDoesNotUseSubjectSubstring(t *testing.T) {
	entry := semanticEntry{
		text:  "annie visited paris",
		terms: map[string]struct{}{"annie": {}, "visit": {}, "paris": {}},
	}
	intent := domain.QueryIntent{Text: "Where did Ann visit?", Subject: "Ann"}
	if entryMatchesIntent(entry, intent, querySemanticTerms(intent)) {
		t.Fatal("subject substring must not match a different entity token")
	}
}

func TestSemanticEntryMatchingDoesNotHardFilterPredicateSurface(t *testing.T) {
	entry := semanticEntry{
		text:  "alice resides in paris",
		terms: semanticTestTermSet("alice resides in paris"),
	}
	intent := domain.QueryIntent{
		Text:      "Where does Alice live?",
		Subject:   "Alice",
		Predicate: "lives",
		Object:    "Paris",
	}
	if !entryMatchesIntent(entry, intent, querySemanticTerms(intent)) {
		t.Fatal("predicate surface mismatch should not hard-filter an otherwise anchored semantic entry")
	}
}

func semanticTestTermSet(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, term := range words.SemanticQueryTerms(text) {
		out[term] = struct{}{}
	}
	return out
}
