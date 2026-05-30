package locomo

import (
	"testing"

	"github.com/GizClaw/flowcraft/eval/dataset"
)

func TestAnalyzeExtractQuality_ClassifiesEvidenceTurns(t *testing.T) {
	ds := &dataset.Dataset{
		Conversations: []dataset.Conversation{{
			ID: "conv-1",
			Turns: []dataset.Turn{
				{Role: "user", EvidenceID: "e1", Content: "Alice booked a flight to Tampa in June."},
				{Role: "user", EvidenceID: "e2", Content: "Bob adopted a golden retriever named Waffles."},
			},
		}},
		Questions: []dataset.Question{{
			ID:             "q1",
			ConversationID: "conv-1",
			EvidenceIDs:    []string{"e1"},
			GoldAnswers:    []string{"Tampa"},
		}},
	}
	facts := []factDumpRecord{{
		Scope: factDumpScope{UserID: "u::conv-1"},
		Facts: []factDumpFact{{
			ID:          "f1",
			Content:     "Alice booked a flight to Tampa in June.",
			Kind:        "event",
			Subject:     "Alice",
			Predicate:   "booked",
			Object:      "flight to Tampa",
			Entities:    []string{"Alice", "Tampa"},
			EvidenceIDs: []string{"e1"},
			ValidFrom:   "2026-06-01",
		}},
	}}
	records := analyzeExtractQuality(ds, facts, extractQualityOptions{})
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	byEvidence := map[string]extractQualityRecord{}
	for _, rec := range records {
		byEvidence[rec.EvidenceID] = rec
	}
	if got := byEvidence["e1"]; got.Status != "ok" || got.TermCoverage < 0.9 || got.FactsCount != 1 {
		t.Fatalf("e1 = %+v", got)
	}
	if got := byEvidence["e2"]; got.Status != "extract_miss" || got.FactsCount != 0 {
		t.Fatalf("e2 = %+v", got)
	}
	if len(byEvidence["e1"].QuestionIDs) != 1 || byEvidence["e1"].QuestionIDs[0] != "q1" {
		t.Fatalf("question refs = %+v", byEvidence["e1"].QuestionIDs)
	}
}

func TestAnalyzeExtractQuality_DetectsPartialAndCompound(t *testing.T) {
	turn := dataset.Turn{Role: "user", EvidenceID: "e1", Content: "Alice visited Tampa, painted a sunrise, and adopted a puppy named Waffles."}
	rec := classifyExtractQualityTurn("conv-1", turn, nil, []factDumpFact{{
		ID:          "f1",
		Content:     "Alice visited Tampa, painted art, adopted a puppy, and bought supplies.",
		Kind:        "event",
		Subject:     "Alice",
		EvidenceIDs: []string{"e1"},
	}})
	if rec.Status != "needs_review" && rec.Status != "partial_coverage" {
		t.Fatalf("status = %q", rec.Status)
	}
	if !containsString(rec.Flags, "possible_compound_fact") {
		t.Fatalf("flags = %+v", rec.Flags)
	}
}
