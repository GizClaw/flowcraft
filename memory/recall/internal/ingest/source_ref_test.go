package ingest

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestSourceEvidenceSpansFromObservationRejectsSpanSourceMismatch(t *testing.T) {
	_, err := SourceEvidenceSpansFromObservation(domain.Observation{
		ID:       "obs-1",
		SourceID: "turn-1",
		Text:     "temperature = 0.2",
		Kind:     domain.ObservationKindTurn,
		Spans: []domain.ObservationSpan{{
			ID:            "span-1",
			ObservationID: "obs-1",
			SourceID:      "other-turn",
			Text:          "temperature = 0.2",
			Start:         0,
			End:           len("temperature = 0.2"),
		}},
	})
	if err == nil {
		t.Fatal("SourceEvidenceSpansFromObservation err = nil, want source mismatch rejection")
	}
}
