package stages

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	linkstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/link"
	observationstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/observation"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
)

func TestLinkExpansionAddsSupportedAssertion(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	temporal := temporalstore.NewMemoryStore()
	observations := observationstore.New()
	links := linkstore.New()
	facts := []domain.TemporalFact{
		{
			ID:      "seed",
			Scope:   scope,
			Kind:    domain.KindNote,
			Content: "Alice favorite drink is tea.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:   "ev-seed",
				Text: "Alice favorite drink is tea.",
			}},
		},
		{
			ID:      "neighbor",
			Scope:   scope,
			Kind:    domain.KindNote,
			Content: "ZXQ-774 calibration capsule note.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:   "ev-neighbor",
				Text: "ZXQ-774 calibration capsule note.",
			}},
		},
	}
	if err := temporal.Append(ctx, facts); err != nil {
		t.Fatalf("temporal.Append: %v", err)
	}
	if err := links.Append(ctx, []domain.FactLink{{
		ID:       "supports",
		Scope:    scope,
		Type:     domain.LinkSupports,
		From:     domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
		To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "neighbor"},
		MergeKey: "supports:seed:neighbor",
	}}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "Alice favorite drink", Limit: 10},
		Plan: &domain.QueryPlan{
			TotalCap: 10,
		},
		MergedItems: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "seed", Scope: scope, Source: "retrieval", Score: 0.9},
			Fact:      facts[0],
			Evidence:  facts[0].EvidenceRefs,
		}},
	}

	detail, err := NewLinkExpansion(temporal, observations, links).Run(ctx, state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, ok := detail.(diagnostic.LinkExpansionDetail)
	if !ok {
		t.Fatalf("detail = %T, want LinkExpansionDetail", detail)
	}
	if got.AddedFacts != 1 || len(state.MergedItems) != 2 {
		t.Fatalf("AddedFacts=%d items=%+v", got.AddedFacts, state.MergedItems)
	}
	added := state.MergedItems[1]
	if added.Fact.ID != "neighbor" || added.Candidate.Source != linkExpansionSource {
		t.Fatalf("added item = %+v", added)
	}
}

func TestLinkExpansionAddsSiblingAssertionThroughObservation(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	temporal := temporalstore.NewMemoryStore()
	observations := observationstore.New()
	links := linkstore.New()
	obs := domain.Observation{
		ID:    "obs-shared",
		Scope: scope,
		Kind:  domain.ObservationKindTurn,
		Text:  "Alice said tea helps, and the ZXQ-774 capsule is stored in the blue box.",
	}
	facts := []domain.TemporalFact{
		{
			ID:      "seed",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Alice",
			Content: "Alice favorite drink is tea.",
		},
		{
			ID:      "sibling",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Alice",
			Content: "Alice stores the ZXQ-774 capsule in the blue box.",
		},
	}
	if err := temporal.Append(ctx, facts); err != nil {
		t.Fatalf("temporal.Append: %v", err)
	}
	if err := observations.Append(ctx, []domain.Observation{obs}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	if err := links.Append(ctx, []domain.FactLink{
		{
			ID:       "seed-derived",
			Scope:    scope,
			Type:     domain.LinkDerivedFrom,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: obs.ID},
			MergeKey: "derived:seed:obs",
		},
		{
			ID:       "seed-supported",
			Scope:    scope,
			Type:     domain.LinkSupports,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: obs.ID},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
			MergeKey: "supports:obs:seed",
		},
		{
			ID:       "sibling-supported",
			Scope:    scope,
			Type:     domain.LinkSupports,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: obs.ID},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "sibling"},
			MergeKey: "supports:obs:sibling",
		},
	}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "Alice ZXQ-774 capsule", Limit: 10},
		Plan:  &domain.QueryPlan{TotalCap: 10},
		MergedItems: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "seed", Scope: scope, Source: "retrieval", Score: 0.9},
			Fact:      facts[0],
		}},
	}

	detail, err := NewLinkExpansion(temporal, observations, links).Run(ctx, state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, ok := detail.(diagnostic.LinkExpansionDetail)
	if !ok {
		t.Fatalf("detail = %T, want LinkExpansionDetail", detail)
	}
	if got.AddedFacts != 1 || len(state.MergedItems) != 2 {
		t.Fatalf("AddedFacts=%d items=%+v", got.AddedFacts, state.MergedItems)
	}
	added := state.MergedItems[1]
	if added.Fact.ID != "sibling" || added.Candidate.Source != linkExpansionSource {
		t.Fatalf("added item = %+v", added)
	}
}

func TestLinkExpansionAddsSiblingAssertionThroughObservationSpanParent(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	temporal := temporalstore.NewMemoryStore()
	observations := observationstore.New()
	links := linkstore.New()
	obs := domain.Observation{
		ID:    "obs-turn",
		Scope: scope,
		Kind:  domain.ObservationKindTurn,
		Text:  "Alice likes tea. The ZXQ-774 capsule is stored in the blue box.",
		Spans: []domain.ObservationSpan{
			{
				ID:            "span-tea",
				ObservationID: "obs-turn",
				Kind:          domain.ObservationSpanKindQuote,
				Text:          "Alice likes tea.",
				Start:         0,
				End:           len("Alice likes tea."),
			},
			{
				ID:            "span-zxq",
				ObservationID: "obs-turn",
				Kind:          domain.ObservationSpanKindQuote,
				Text:          "The ZXQ-774 capsule is stored in the blue box.",
				Start:         len("Alice likes tea. "),
				End:           len("Alice likes tea. The ZXQ-774 capsule is stored in the blue box."),
			},
			{
				ID:            "span-other",
				ObservationID: "obs-turn",
				Kind:          domain.ObservationSpanKindQuote,
				Text:          "Alice likes unrelated watercolor classes.",
				Start:         len("Alice likes tea. "),
				End:           len("Alice likes tea. The ZXQ-774 capsule is stored in the blue box."),
			},
		},
	}
	facts := []domain.TemporalFact{
		{
			ID:      "seed",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Alice",
			Content: "Alice likes tea.",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-turn",
				SpanID:        "span-tea",
				Text:          "Alice likes tea.",
			}},
		},
		{
			ID:      "sibling",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Alice",
			Content: "The capsule is stored in the blue box.",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-turn",
				SpanID:        "span-zxq",
				Text:          "The ZXQ-774 capsule is stored in the blue box.",
			}},
		},
		{
			ID:      "pollution",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Alice",
			Content: "Alice likes unrelated watercolor classes.",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-turn",
				SpanID:        "span-other",
				Text:          "Alice likes unrelated watercolor classes.",
			}},
		},
	}
	if err := temporal.Append(ctx, facts); err != nil {
		t.Fatalf("temporal.Append: %v", err)
	}
	if err := observations.Append(ctx, []domain.Observation{obs}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	if err := links.Append(ctx, []domain.FactLink{
		{
			ID:       "seed-derived",
			Scope:    scope,
			Type:     domain.LinkDerivedFrom,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: "span-tea"},
			MergeKey: "derived:seed:span-tea",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-turn",
				SpanID:        "span-tea",
				Text:          "Alice likes tea.",
			}},
		},
		{
			ID:       "seed-supported",
			Scope:    scope,
			Type:     domain.LinkSupports,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: "span-tea"},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
			MergeKey: "supports:span-tea:seed",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-turn",
				SpanID:        "span-tea",
				Text:          "Alice likes tea.",
			}},
		},
		{
			ID:       "sibling-supported",
			Scope:    scope,
			Type:     domain.LinkSupports,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: "span-zxq"},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "sibling"},
			MergeKey: "supports:span-zxq:sibling",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-turn",
				SpanID:        "span-zxq",
				Text:          "The ZXQ-774 capsule is stored in the blue box.",
			}},
		},
		{
			ID:       "pollution-supported",
			Scope:    scope,
			Type:     domain.LinkSupports,
			From:     domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: "span-other"},
			To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "pollution"},
			MergeKey: "supports:span-other:pollution",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-turn",
				SpanID:        "span-other",
				Text:          "Alice likes unrelated watercolor classes.",
			}},
		},
	}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "ZXQ-774 capsule blue box", Limit: 10},
		Plan:  &domain.QueryPlan{TotalCap: 10},
		MergedItems: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "seed", Scope: scope, Source: "retrieval", Score: 0.9},
			Fact:      facts[0],
			Evidence:  facts[0].EvidenceRefs,
		}},
	}

	detail, err := NewLinkExpansion(temporal, observations, links).Run(ctx, state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, ok := detail.(diagnostic.LinkExpansionDetail)
	if !ok {
		t.Fatalf("detail = %T, want LinkExpansionDetail", detail)
	}
	if got.AddedFacts != 1 || len(state.MergedItems) != 2 {
		t.Fatalf("AddedFacts=%d items=%+v", got.AddedFacts, state.MergedItems)
	}
	if state.MergedItems[1].Fact.ID != "sibling" {
		t.Fatalf("added fact = %q, want sibling", state.MergedItems[1].Fact.ID)
	}
}
