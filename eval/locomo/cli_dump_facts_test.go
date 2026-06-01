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
	rec := newV2FactsDump(time.Now(), runners.Scope{
		RuntimeID: "locomo",
		UserID:    "user::conv-1",
		AgentID:   "agent",
	}, recall.SaveRequest{Turns: []recall.TurnContext{{
		ID:         "e1",
		EvidenceID: "e1",
		SessionID:  "session_1",
		Text:       "I booked a flight to Tampa.",
	}}}, []recall.TemporalFact{{
		ID:               "f1",
		Kind:             recall.FactEvent,
		Content:          "Alice booked a flight to Tampa.",
		Subject:          "Alice",
		Predicate:        "booked",
		Object:           "flight to Tampa",
		Polarity:         recall.PolarityNegated,
		Modality:         recall.ModalityCanceled,
		Certainty:        recall.CertaintyUncertain,
		Entities:         []string{"Alice", "Tampa"},
		SourceMessageIDs: []string{"m1"},
		ValidFrom:        &validFrom,
		EvidenceRefs: []recall.EvidenceRef{{
			ID:   "e1",
			Role: "user",
			Text: "I booked a flight to Tampa.",
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
	if len(fact.Entities) != 2 || fact.Entities[1] != "Tampa" {
		t.Fatalf("entities = %+v", fact.Entities)
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
