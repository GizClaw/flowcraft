package graphledger

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	linkstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/link"
	observationstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/observation"
)

func TestClearAssertionKeepsObservationUsedByAnotherSpan(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	observations := observationstore.New()
	links := linkstore.New()
	obs := domain.Observation{
		ID:       "obs-1",
		Scope:    scope,
		Kind:     domain.ObservationKindTurn,
		SourceID: "msg-1",
		Text:     "Alice likes tea and Bob likes coffee.",
		Spans: []domain.ObservationSpan{
			{ID: "span-tea", ObservationID: "obs-1", SourceID: "msg-1", Kind: domain.ObservationSpanKindQuote, Text: "Alice likes tea"},
			{ID: "span-coffee", ObservationID: "obs-1", SourceID: "msg-1", Kind: domain.ObservationSpanKindQuote, Text: "Bob likes coffee"},
		},
	}
	if err := observations.Append(ctx, []domain.Observation{obs}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)
	if err := links.Append(ctx, []domain.FactLink{
		NewFactObservationSpanLink(scope, domain.LinkDerivedFrom, "fact-tea", "span-tea", []domain.EvidenceRef{{ObservationID: "obs-1", SpanID: "span-tea"}}, now),
		NewObservationSpanFactLink(scope, domain.LinkSupports, "span-tea", "fact-tea", []domain.EvidenceRef{{ObservationID: "obs-1", SpanID: "span-tea"}}, now),
		NewFactObservationSpanLink(scope, domain.LinkDerivedFrom, "fact-coffee", "span-coffee", []domain.EvidenceRef{{ObservationID: "obs-1", SpanID: "span-coffee"}}, now),
		NewObservationSpanFactLink(scope, domain.LinkSupports, "span-coffee", "fact-coffee", []domain.EvidenceRef{{ObservationID: "obs-1", SpanID: "span-coffee"}}, now),
	}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}

	if _, _, _, err := ClearAssertion(ctx, scope, "fact-tea", observations, links); err != nil {
		t.Fatalf("ClearAssertion: %v", err)
	}
	if _, err := observations.Get(ctx, scope, "obs-1"); err != nil {
		t.Fatalf("shared observation was deleted: %v", err)
	}
	remaining, err := links.FindByNode(ctx, scope, domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: "span-coffee"})
	if err != nil {
		t.Fatalf("FindByNode: %v", err)
	}
	if len(remaining) == 0 {
		t.Fatal("remaining fact lost its span links")
	}
}

type failingSpanLookupLinks struct {
	port.LinkStore
}

func (f failingSpanLookupLinks) FindByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) ([]domain.FactLink, error) {
	if node.Kind == domain.GraphNodeObservationSpan {
		return nil, context.DeadlineExceeded
	}
	return f.LinkStore.FindByNode(ctx, scope, node)
}

func TestClearAssertionPropagatesSpanLookupError(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	observations := observationstore.New()
	links := linkstore.New()
	obs := domain.Observation{
		ID:       "obs-1",
		Scope:    scope,
		Kind:     domain.ObservationKindTurn,
		SourceID: "msg-1",
		Text:     "Alice likes tea.",
		Spans: []domain.ObservationSpan{{
			ID:            "span-tea",
			ObservationID: "obs-1",
			SourceID:      "msg-1",
			Kind:          domain.ObservationSpanKindQuote,
			Text:          "Alice likes tea",
		}},
	}
	if err := observations.Append(ctx, []domain.Observation{obs}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	now := time.Date(2026, 6, 1, 1, 2, 3, 0, time.UTC)
	if err := links.Append(ctx, []domain.FactLink{
		NewFactObservationSpanLink(scope, domain.LinkDerivedFrom, "fact-tea", "span-tea", []domain.EvidenceRef{{ObservationID: "obs-1", SpanID: "span-tea"}}, now),
		NewObservationSpanFactLink(scope, domain.LinkSupports, "span-tea", "fact-tea", []domain.EvidenceRef{{ObservationID: "obs-1", SpanID: "span-tea"}}, now),
	}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}

	_, _, _, err := ClearAssertion(ctx, scope, "fact-tea", observations, failingSpanLookupLinks{LinkStore: links})
	if err == nil {
		t.Fatal("ClearAssertion must propagate span lookup errors")
	}
}
