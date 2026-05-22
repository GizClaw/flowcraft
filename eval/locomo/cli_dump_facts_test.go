package locomo

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
)

func TestNewV2FactsDump_IncludesAuditFields(t *testing.T) {
	validFrom := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	rec := newV2FactsDump(time.Now(), runners.Scope{
		RuntimeID: "locomo",
		UserID:    "user::conv-1",
		AgentID:   "agent",
	}, []recall.TemporalFact{{
		ID:               "f1",
		Kind:             recall.FactEvent,
		Content:          "Alice booked a flight to Tampa.",
		Subject:          "Alice",
		Predicate:        "booked",
		Object:           "flight to Tampa",
		Entities:         []string{"Alice", "Tampa"},
		SourceMessageIDs: []string{"m1"},
		ValidFrom:        &validFrom,
		EvidenceRefs: []recall.EvidenceRef{{
			ID:   "e1",
			Role: "user",
			Text: "I booked a flight to Tampa.",
		}},
	}})
	if rec.Runner != "flowcraft-recall-v2" {
		t.Fatalf("runner = %q", rec.Runner)
	}
	if rec.Scope.UserID != "user::conv-1" {
		t.Fatalf("scope = %+v", rec.Scope)
	}
	if len(rec.Facts) != 1 {
		t.Fatalf("facts = %+v", rec.Facts)
	}
	fact := rec.Facts[0]
	if fact.ID != "f1" || fact.Kind != "event" || fact.ValidFrom != "2026-05-21" {
		t.Fatalf("fact core fields = %+v", fact)
	}
	if len(fact.EvidenceIDs) != 1 || fact.EvidenceIDs[0] != "e1" {
		t.Fatalf("evidence ids = %+v", fact.EvidenceIDs)
	}
	if len(fact.Entities) != 2 || fact.Entities[1] != "Tampa" {
		t.Fatalf("entities = %+v", fact.Entities)
	}
}
