package graphledger

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestObservationFromEvidenceRefSharesTurnSourceObservationID(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)

	turnObs := ObservationFromTurn(scope, domain.TurnContext{
		ID:         "msg-1",
		EvidenceID: "source-1",
		Text:       "Alice said the ZXQ-774 capsule is in the blue box.",
	}, 0, now, now, "req-1")
	evidenceObs := ObservationFromEvidenceRef(scope, domain.EvidenceRef{
		ID:        "source-1",
		MessageID: "msg-1",
		Text:      "the ZXQ-774 capsule is in the blue box",
	}, "fact-1", 0, now, "req-1")

	if turnObs.ID == "" {
		t.Fatal("turn observation id is empty")
	}
	if turnObs.ID != evidenceObs.ID {
		t.Fatalf("observation ids differ: turn=%q evidence=%q", turnObs.ID, evidenceObs.ID)
	}
	if len(evidenceObs.Spans) != 1 || evidenceObs.Spans[0].ObservationID != evidenceObs.ID {
		t.Fatalf("evidence observation span not anchored to parent: %#v", evidenceObs.Spans)
	}
}

func TestObservationFromEvidenceRefFallsBackWhenSourceMissing(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)

	left := ObservationFromEvidenceRef(scope, domain.EvidenceRef{Text: "left"}, "fact-1", 0, now, "")
	right := ObservationFromEvidenceRef(scope, domain.EvidenceRef{Text: "left"}, "fact-2", 0, now, "")

	if left.ID == "" || right.ID == "" {
		t.Fatalf("fallback ids must be non-empty: left=%q right=%q", left.ID, right.ID)
	}
	if left.ID == right.ID {
		t.Fatalf("source-less evidence refs should remain fact-scoped, got %q", left.ID)
	}
}

func TestBuildDeltaLinksAssertionToObservationSpan(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)
	fact := domain.TemporalFact{
		ID:      "fact-1",
		Scope:   scope,
		Content: "Alice placed ZXQ-774 in the blue box.",
		EvidenceRefs: []domain.EvidenceRef{{
			ID:        "source-1",
			MessageID: "msg-1",
			Text:      "ZXQ-774 in the blue box",
		}},
	}

	delta := BuildDelta(scope, []domain.TemporalFact{fact}, nil, nil, now, now, "req-1")

	if len(delta.Observations) != 1 || len(delta.Observations[0].Spans) != 1 {
		t.Fatalf("delta observations = %#v", delta.Observations)
	}
	spanID := delta.Observations[0].Spans[0].ID
	if spanID == "" {
		t.Fatal("span id is empty")
	}
	for _, link := range delta.Links {
		if link.From.Kind == domain.GraphNodeAssertion && link.To.Kind == domain.GraphNodeObservationSpan && link.To.ID == spanID {
			if len(link.EvidenceRefs) != 1 || link.EvidenceRefs[0].SpanID != spanID {
				t.Fatalf("link evidence refs not normalized: %#v", link.EvidenceRefs)
			}
			return
		}
	}
	t.Fatalf("missing assertion -> observation_span link in %#v", delta.Links)
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
				ID:        "source-1",
				MessageID: "msg-1",
				Text:      "ZXQ-774 in the blue box",
			}},
		},
		{
			ID:      "fact-2",
			Scope:   scope,
			Content: "Alice said the capsule is fragile.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:        "source-1",
				MessageID: "msg-1",
				Text:      "capsule is fragile",
			}},
		},
	}

	delta := BuildDelta(scope, facts, nil, nil, now, now, "req-1")
	if len(delta.Observations) != 1 {
		t.Fatalf("delta observations = %#v, want one shared observation", delta.Observations)
	}
	if len(delta.Observations[0].Spans) != 2 {
		t.Fatalf("merged spans = %#v, want both evidence spans", delta.Observations[0].Spans)
	}
	spanIDs := map[string]bool{}
	for _, span := range delta.Observations[0].Spans {
		spanIDs[span.ID] = true
	}
	for _, link := range delta.Links {
		if link.To.Kind == domain.GraphNodeObservationSpan && !spanIDs[link.To.ID] {
			t.Fatalf("link points at missing span %q; spans=%#v link=%#v", link.To.ID, delta.Observations[0].Spans, link)
		}
		if link.From.Kind == domain.GraphNodeObservationSpan && !spanIDs[link.From.ID] {
			t.Fatalf("link points from missing span %q; spans=%#v link=%#v", link.From.ID, delta.Observations[0].Spans, link)
		}
	}
}
