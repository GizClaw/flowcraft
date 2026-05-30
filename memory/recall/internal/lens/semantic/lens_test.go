package semantic

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

func TestProjectionKindMatchesOnlyExplicitSemanticSignals(t *testing.T) {
	subjectOnly := domain.TemporalFact{Subject: "Mira"}
	if projectionKindMatches(planner.SourceAssertion, subjectOnly) {
		t.Fatal("assertion projection must not index subject-only facts")
	}

	triple := domain.TemporalFact{Subject: "Mira", Predicate: "visited", Object: "Paris"}
	if !projectionKindMatches(planner.SourceAssertion, triple) {
		t.Fatal("assertion projection should index complete assertion triples")
	}

	negated := domain.TemporalFact{Content: "Mira did not visit Paris.", Polarity: domain.PolarityNegated}
	if !projectionKindMatches(planner.SourceAssertion, negated) {
		t.Fatal("assertion projection should index explicit assertion metadata")
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
