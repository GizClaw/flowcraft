package locomo

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
)

func TestNewV2FactsDump_IncludesAuditFields(t *testing.T) {
	validFrom := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	observedAt := time.Date(2026, 5, 20, 9, 30, 0, 0, time.UTC)
	rec := newV2FactsDump(time.Now(), runners.Scope{
		RuntimeID: "locomo",
		UserID:    "user::conv-1",
		AgentID:   "agent",
	}, recall.SaveRequest{Turns: []recall.TurnContext{{
		ID:         "e1",
		EvidenceID: "e1",
		SessionID:  "session_1",
		Role:       "user",
		Speaker:    "Alice",
		Time:       observedAt,
		Text:       "I booked a flight to Tampa.",
	}}}, []recall.TemporalFact{{
		ID:               "f1",
		Kind:             recall.FactEvent,
		Content:          "Alice booked a flight to Tampa.",
		Subject:          "Alice",
		Predicate:        "booked",
		Object:           "flight to Tampa",
		Location:         "Tampa",
		Polarity:         recall.PolarityNegated,
		Modality:         recall.ModalityCanceled,
		Certainty:        recall.CertaintyUncertain,
		Entities:         []string{"Alice", "Tampa"},
		Participants:     []string{"Alice"},
		SourceMessageIDs: []string{"m1"},
		ObservedAt:       observedAt,
		ValidFrom:        &validFrom,
		EvidenceRefs: []recall.EvidenceRef{{
			ID:        "e1",
			MessageID: "m1",
			Role:      "user",
			Text:      "I booked a flight to Tampa.",
			Timestamp: observedAt,
		}},
	}}, &diagnostics.SaveDiagnostics{ExtractorTokenUsage: diagnostics.ExtractorTokenUsage{
		Calls:                 2,
		TokenUsage:            diagnostics.TokenUsage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140},
		AvgTotalTokensPerCall: 70,
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
	if fact.Polarity != "negated" || fact.Modality != "canceled" || fact.Certainty != "uncertain" {
		t.Fatalf("semantic assertion fields = %+v", fact)
	}
	if len(fact.EvidenceIDs) != 1 || fact.EvidenceIDs[0] != "e1" {
		t.Fatalf("evidence ids = %+v", fact.EvidenceIDs)
	}
	if len(fact.EvidenceRefs) != 1 || fact.EvidenceRefs[0].Timestamp == "" || fact.EvidenceRefs[0].MessageID != "m1" {
		t.Fatalf("evidence refs = %+v", fact.EvidenceRefs)
	}
	if fact.ObservedAt == "" {
		t.Fatalf("observed_at missing: %+v", fact)
	}
	if len(fact.Entities) != 2 || fact.Entities[1] != "Tampa" {
		t.Fatalf("entities = %+v", fact.Entities)
	}
	if fact.Location != "Tampa" || len(fact.Participants) != 1 || fact.Participants[0] != "Alice" {
		t.Fatalf("location/participants = %+v", fact)
	}
	if rec.ExtractTokens == nil || rec.ExtractTokens.TotalTokens != 140 || rec.ExtractTokens.AvgTotalTokensPerCall != 70 {
		t.Fatalf("extract token usage = %+v", rec.ExtractTokens)
	}
	if rec.Batch == nil || rec.Batch.ConversationID != "conv-1" || rec.Batch.SessionID != "session_1" || rec.Batch.TurnCount != 1 {
		t.Fatalf("batch metadata = %+v", rec.Batch)
	}
	if len(rec.Batch.EvidenceIDs) != 1 || rec.Batch.EvidenceIDs[0] != "e1" {
		t.Fatalf("batch evidence ids = %+v", rec.Batch.EvidenceIDs)
	}
	if len(rec.Batch.Turns) != 1 || rec.Batch.Turns[0].Text != "I booked a flight to Tampa." || rec.Batch.Turns[0].Timestamp == "" {
		t.Fatalf("batch turns = %+v", rec.Batch.Turns)
	}
}

func TestTemporalFactsFromDumpRestoresEvidenceAndSemantics(t *testing.T) {
	validFrom := "2026-05-21"
	facts, err := temporalFactsFromDump([]factDumpFact{{
		Content:      "Alice canceled a flight to Tampa.",
		Kind:         "event",
		Subject:      "Alice",
		Predicate:    "canceled",
		Object:       "flight to Tampa",
		Location:     "Tampa",
		Polarity:     "negated",
		Modality:     "canceled",
		Certainty:    "explicit",
		Entities:     []string{"Alice", "Tampa"},
		Participants: []string{"Alice"},
		ObservedAt:   "2026-05-20T09:30:00Z",
		EvidenceIDs:  []string{"conv-1:D1:7"},
		EvidenceRefs: []factDumpEvidenceRef{{
			ID:        "conv-1:D1:7",
			MessageID: "msg-7",
			Role:      "user",
			Text:      "I canceled the flight to Tampa.",
			Timestamp: "2026-05-20T09:30:00Z",
		}},
		SourceMessageIDs: []string{"msg-7"},
		EvidenceText:     "I canceled the flight to Tampa.",
		ValidFrom:        validFrom,
		Confidence:       0.9,
	}})
	if err != nil {
		t.Fatalf("temporalFactsFromDump: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %+v", facts)
	}
	f := facts[0]
	if f.Kind != recall.FactEvent || f.Polarity != recall.PolarityNegated || f.Modality != recall.ModalityCanceled {
		t.Fatalf("semantic fields not restored: %+v", f)
	}
	if f.Location != "Tampa" || len(f.Participants) != 1 || f.Participants[0] != "Alice" {
		t.Fatalf("structured fields not restored: %+v", f)
	}
	if f.ValidFrom == nil {
		t.Fatalf("valid_from missing")
	}
	if f.ObservedAt.IsZero() {
		t.Fatalf("observed_at = %v", f.ObservedAt)
	}
	if got := f.ObservedAt.Format(time.RFC3339); got != "2026-05-20T09:30:00Z" {
		t.Fatalf("observed_at = %v", f.ObservedAt)
	}
	if got := f.ValidFrom.Format("2006-01-02"); got != validFrom {
		t.Fatalf("valid_from = %v, want %s", f.ValidFrom, validFrom)
	}
	if len(f.EvidenceRefs) != 1 || f.EvidenceRefs[0].ID != "conv-1:D1:7" || f.EvidenceRefs[0].MessageID != "msg-7" {
		t.Fatalf("evidence refs = %+v", f.EvidenceRefs)
	}
	if f.EvidenceRefs[0].Text != "I canceled the flight to Tampa." {
		t.Fatalf("evidence text = %q", f.EvidenceRefs[0].Text)
	}
}

func TestTurnContextsFromDumpRecordRestoresSourceTurns(t *testing.T) {
	rec := factDumpRecord{Batch: &factDumpBatch{Turns: []factDumpTurn{{
		ID:         "msg-1",
		EvidenceID: "D1:1",
		SessionID:  "session_1",
		Role:       "user",
		Speaker:    "Alice",
		Text:       "I booked a flight to Tampa.",
		Timestamp:  "2026-05-20T09:30:00Z",
	}}}}
	turns, observedAt, err := turnContextsFromDumpRecord(rec)
	if err != nil {
		t.Fatalf("turnContextsFromDumpRecord: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %+v", turns)
	}
	if turns[0].ID != "msg-1" || turns[0].EvidenceID != "D1:1" || turns[0].Speaker != "Alice" {
		t.Fatalf("turn = %+v", turns[0])
	}
	if got := observedAt.Format(time.RFC3339); got != "2026-05-20T09:30:00Z" {
		t.Fatalf("observedAt = %v", observedAt)
	}
}

func TestValidateFactDumpEvidenceIDsRejectsOutsideBatchRef(t *testing.T) {
	rec := factDumpRecord{
		Batch: &factDumpBatch{
			EvidenceIDs:      []string{"D1:1"},
			SourceMessageIDs: []string{"msg-1"},
			Turns: []factDumpTurn{{
				ID:         "msg-1",
				EvidenceID: "D1:1",
				Text:       "source",
			}},
		},
		Facts: []factDumpFact{{
			Content: "Alice visited Riverton.",
			EvidenceRefs: []factDumpEvidenceRef{{
				ID:        "D1:404",
				MessageID: "msg-1",
				Text:      "outside",
			}},
		}},
	}
	if err := validateFactDumpEvidenceIDs(rec); err == nil {
		t.Fatal("expected outside evidence id to be rejected")
	}
}

func TestValidateFactDumpEvidenceIDsRejectsMixedRefAliases(t *testing.T) {
	rec := factDumpRecord{
		Batch: &factDumpBatch{Turns: []factDumpTurn{
			{ID: "msg-1", EvidenceID: "D1:1", Text: "first"},
			{ID: "msg-2", EvidenceID: "D1:2", Text: "second"},
		}},
		Facts: []factDumpFact{{
			Content: "Alice visited Riverton.",
			EvidenceRefs: []factDumpEvidenceRef{{
				ID:        "D1:1",
				MessageID: "msg-2",
				Text:      "mixed",
			}},
		}},
	}
	if err := validateFactDumpEvidenceIDs(rec); err == nil {
		t.Fatal("expected mixed evidence aliases to be rejected")
	}
}

func TestNewV2FactsDumpSummary_AveragesPerExtract(t *testing.T) {
	var stats factDumpTokenStats
	stats.Add(diagnostics.ExtractorTokenUsage{
		Calls:      2,
		TokenUsage: diagnostics.TokenUsage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140},
	})
	stats.Add(diagnostics.ExtractorTokenUsage{
		Calls:      3,
		TokenUsage: diagnostics.TokenUsage{InputTokens: 200, OutputTokens: 80, TotalTokens: 280},
	})
	rec := newV2FactsDumpSummary(time.Now(), stats)
	if rec.Type != "extract_token_summary" || rec.ExtractCount != 2 {
		t.Fatalf("summary metadata = %+v", rec)
	}
	if rec.ExtractTokens == nil || rec.ExtractTokens.TotalTokens != 420 {
		t.Fatalf("summary total = %+v", rec.ExtractTokens)
	}
	if rec.AvgExtractTokens == nil || rec.AvgExtractTokens.TotalTokens != 210 {
		t.Fatalf("summary average = %+v", rec.AvgExtractTokens)
	}
}
