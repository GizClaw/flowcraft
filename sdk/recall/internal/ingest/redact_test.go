package ingest_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
)

func TestRedactDroppedFacts_StripsPayloadKeepsHashes(t *testing.T) {
	drops := []diagnostic.DroppedFact{{
		Fact: domain.TemporalFact{
			ID: "f1", Kind: domain.KindState, Content: "secret content",
		},
		Reason: "policy:reject",
	}}
	got := ingest.RedactDroppedFacts(drops)
	if got[0].Fact != nil {
		t.Fatalf("Fact payload must be cleared, got %T", got[0].Fact)
	}
	if got[0].FactID != "f1" || got[0].Kind != string(domain.KindState) || got[0].ContentHash == "" {
		t.Fatalf("redacted drop = %+v", got[0])
	}
}
