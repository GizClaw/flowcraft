package graphledger

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestBuildDeltaLinksAssertionToObservationSpan(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)
	fact := domain.TemporalFact{
		ID:      "fact-1",
		Scope:   scope,
		Content: "Alice placed ZXQ-774 in the blue box.",
		EvidenceRefs: []domain.EvidenceRef{{
			ID:            "source-1",
			MessageID:     "msg-1",
			ObservationID: "obs-1",
			SpanID:        "span-1",
			Text:          "ZXQ-774 in the blue box",
		}},
	}

	delta := BuildDelta(scope, []domain.TemporalFact{fact}, nil, nil, now, now, "req-1")

	if len(delta.Observations) != 0 {
		t.Fatalf("delta observations = %#v, want no synthesized observations", delta.Observations)
	}
	for _, link := range delta.Links {
		if link.From.Kind == domain.GraphNodeAssertion && link.To.Kind == domain.GraphNodeObservationSpan && link.To.ID == "span-1" {
			if len(link.EvidenceRefs) != 1 || link.EvidenceRefs[0].SpanID != "span-1" {
				t.Fatalf("link evidence refs not normalized: %#v", link.EvidenceRefs)
			}
			return
		}
	}
	t.Fatalf("missing assertion -> observation_span link in %#v", delta.Links)
}

func TestBuildDelta_DoesNotEmitAnswersSlotForSingleAssertionParameter(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)
	fact := domain.TemporalFact{
		ID:    "fact-param",
		Scope: scope,
		Kind:  domain.KindParameter,
		EvidenceRefs: []domain.EvidenceRef{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			Text:          "mode = fast",
		}},
	}
	delta := BuildDelta(scope, []domain.TemporalFact{fact}, nil, nil, now, now, "req-1")
	for _, link := range delta.Links {
		if link.Type == domain.LinkAnswersSlot {
			t.Fatalf("answers_slot should not be emitted for single-assertion parameter facts: %+v", link)
		}
	}
}

func TestBuildDeltaMergesSpansForSharedObservation(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)
	facts := []domain.TemporalFact{
		{
			ID:      "fact-1",
			Scope:   scope,
			Content: "Alice put ZXQ-774 in the blue box.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:            "source-1",
				MessageID:     "msg-1",
				ObservationID: "obs-1",
				SpanID:        "span-1",
				Text:          "ZXQ-774 in the blue box",
			}},
		},
		{
			ID:      "fact-2",
			Scope:   scope,
			Content: "Alice said the capsule is fragile.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:            "source-1",
				MessageID:     "msg-1",
				ObservationID: "obs-1",
				SpanID:        "span-2",
				Text:          "capsule is fragile",
			}},
		},
	}

	delta := BuildDelta(scope, facts, nil, nil, now, now, "req-1")
	if len(delta.Observations) != 0 {
		t.Fatalf("delta observations = %#v, want no synthesized observations", delta.Observations)
	}
	sawSameObservation := false
	for _, link := range delta.Links {
		if link.Type == domain.LinkSameEventAs {
			t.Fatalf("same_event_as must not be inferred from shared observation alone: %+v", link)
		}
		if link.Type == domain.LinkSameObservation {
			sawSameObservation = true
		}
	}
	if !sawSameObservation {
		t.Fatalf("missing same_observation link in %#v", delta.Links)
	}
}

func TestBuildDeltaDoesNotSynthesizeBareEvidenceSpans(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)
	fact := domain.TemporalFact{
		ID:      "fact-1",
		Scope:   scope,
		Kind:    domain.KindNote,
		Content: "Alice likes tea.",
		EvidenceRefs: []domain.EvidenceRef{{
			ID:   "source-1",
			Text: "Alice likes tea",
		}},
	}

	delta := BuildDelta(scope, []domain.TemporalFact{fact}, nil, nil, now, now, "req-1")
	if len(delta.Observations) != 0 {
		t.Fatalf("bare evidence refs must not synthesize observations: %#v", delta.Observations)
	}
	if len(delta.Links) != 0 {
		t.Fatalf("bare evidence refs must not synthesize graph source links: %#v", delta.Links)
	}
}
