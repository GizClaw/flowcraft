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

func TestLinkExpansionDoesNotAddSiblingOnSparseTokenOverlap(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	temporal := temporalstore.NewMemoryStore()
	observations := observationstore.New()
	links := linkstore.New()
	obs := domain.Observation{
		ID:    "obs-shared",
		Scope: scope,
		Kind:  domain.ObservationKindTurn,
		Text:  "Alice discussed Paris. Alice likes tea.",
	}
	facts := []domain.TemporalFact{
		{
			ID:      "seed",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Alice",
			Content: "Alice visited Paris.",
		},
		{
			ID:      "sibling",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Alice",
			Content: "Alice likes tea.",
		},
	}
	if err := temporal.Append(ctx, facts); err != nil {
		t.Fatalf("temporal.Append: %v", err)
	}
	if err := observations.Append(ctx, []domain.Observation{obs}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	if err := links.Append(ctx, []domain.FactLink{
		{ID: "seed-derived", Scope: scope, Type: domain.LinkDerivedFrom, From: domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"}, To: domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: obs.ID}, MergeKey: "derived:seed:obs"},
		{ID: "sibling-supported", Scope: scope, Type: domain.LinkSupports, From: domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: obs.ID}, To: domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "sibling"}, MergeKey: "supports:obs:sibling"},
	}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "What did Alice like in Paris?", Limit: 10},
		Plan: &domain.QueryPlan{
			TotalCap: 10,
			Intent: domain.QueryIntent{Features: domain.QueryFeatures{
				Tokens: map[string]struct{}{"what": {}, "alice": {}, "like": {}, "paris": {}},
			}},
		},
		MergedItems: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "seed", Scope: scope, Source: "retrieval", Score: 0.9},
			Fact:      facts[0],
		}},
	}

	detail, err := NewLinkExpansion(temporal, observations, links).Run(ctx, state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := detail.(diagnostic.LinkExpansionDetail)
	if got.AddedFacts != 0 || len(state.MergedItems) != 1 {
		t.Fatalf("sparse lexical overlap should not add sibling: detail=%+v items=%+v", got, state.MergedItems)
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

func TestLinkExpansionAddsAdjacentEvidenceBridgeAssertion(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	temporal := temporalstore.NewMemoryStore()
	observations := observationstore.New()
	links := linkstore.New()
	obsAnswer := domain.Observation{
		ID:        "obs-answer",
		Scope:     scope,
		Kind:      domain.ObservationKindEvidence,
		SourceID:  "conv:D7:11",
		SessionID: "session-7",
		MessageID: "conv:D7:11",
		Text:      "Becoming Nicole.",
		Spans: []domain.ObservationSpan{{
			ID:            "span-answer",
			ObservationID: "obs-answer",
			SourceID:      "conv:D7:11",
			Kind:          domain.ObservationSpanKindQuote,
			Text:          "Becoming Nicole.",
		}},
	}
	facts := []domain.TemporalFact{
		{
			ID:      "seed",
			Scope:   scope,
			Kind:    domain.KindState,
			Subject: "Melanie",
			Content: "Melanie is reading the book that Caroline recommended.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:        "conv:D7:10",
				MessageID: "conv:D7:10",
				SessionID: "session-7",
				Text:      "I'm reading that book Caroline recommended.",
			}},
		},
		{
			ID:      "title",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Melanie",
			Content: "Melanie read Becoming Nicole from Caroline's recommendation.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:            "conv:D7:11",
				MessageID:     "conv:D7:11",
				SessionID:     "session-7",
				ObservationID: "obs-answer",
				SpanID:        "span-answer",
				Text:          "Becoming Nicole.",
			}},
		},
	}
	if err := temporal.Append(ctx, facts); err != nil {
		t.Fatalf("temporal.Append: %v", err)
	}
	if err := observations.Append(ctx, []domain.Observation{obsAnswer}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	if err := links.Append(ctx, []domain.FactLink{{
		ID:       "title-supported",
		Scope:    scope,
		Type:     domain.LinkSupports,
		From:     domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: "span-answer"},
		To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "title"},
		MergeKey: "supports:span-answer:title",
		EvidenceRefs: []domain.EvidenceRef{{
			ID:            "conv:D7:11",
			MessageID:     "conv:D7:11",
			SessionID:     "session-7",
			ObservationID: "obs-answer",
			SpanID:        "span-answer",
			Text:          "Becoming Nicole.",
		}},
	}}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "What book did Melanie read from Caroline's suggestion?", Limit: 10},
		Plan: &domain.QueryPlan{
			TotalCap:    10,
			TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskBridgeResolution},
			Intent: domain.QueryIntent{Features: domain.QueryFeatures{
				Proper: map[string]struct{}{"melanie": {}, "caroline": {}},
			}},
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
	got := detail.(diagnostic.LinkExpansionDetail)
	if got.AddedFacts != 1 || len(state.MergedItems) != 2 || state.MergedItems[1].Fact.ID != "title" {
		t.Fatalf("adjacent evidence bridge should add title fact: detail=%+v items=%+v", got, state.MergedItems)
	}
	if got.AdjacentBridgeRefs == 0 ||
		got.AdjacentBridgeObservationScans == 0 ||
		got.AdjacentBridgeMatchedObservations != 1 ||
		got.AdjacentBridgeScannedLinks == 0 ||
		got.AdjacentBridgeAddedFacts != 1 ||
		len(got.AdjacentBridgeAddedFactIDs) != 1 ||
		got.AdjacentBridgeAddedFactIDs[0] != "title" {
		t.Fatalf("adjacent bridge diagnostics missing detail: %+v", got)
	}
}

func TestLinkExpansionAdjacentEvidenceBridgeRequiresSameSession(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	temporal := temporalstore.NewMemoryStore()
	observations := observationstore.New()
	links := linkstore.New()
	obs := domain.Observation{
		ID:        "obs-other-session",
		Scope:     scope,
		Kind:      domain.ObservationKindEvidence,
		SourceID:  "conv:D7:11",
		SessionID: "session-other",
		MessageID: "conv:D7:11",
		Text:      "Becoming Nicole.",
		Spans: []domain.ObservationSpan{{
			ID:            "span-other-session",
			ObservationID: "obs-other-session",
			SourceID:      "conv:D7:11",
			Kind:          domain.ObservationSpanKindQuote,
			Text:          "Becoming Nicole.",
		}},
	}
	facts := []domain.TemporalFact{
		{
			ID:      "seed",
			Scope:   scope,
			Kind:    domain.KindState,
			Subject: "Melanie",
			Content: "Melanie is reading the book that Caroline recommended.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:        "conv:D7:10",
				MessageID: "conv:D7:10",
				SessionID: "session-7",
				Text:      "I'm reading that book Caroline recommended.",
			}},
		},
		{
			ID:      "title",
			Scope:   scope,
			Kind:    domain.KindNote,
			Subject: "Melanie",
			Content: "Melanie read Becoming Nicole from Caroline's recommendation.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:            "conv:D7:11",
				MessageID:     "conv:D7:11",
				SessionID:     "session-other",
				ObservationID: "obs-other-session",
				SpanID:        "span-other-session",
				Text:          "Becoming Nicole.",
			}},
		},
	}
	if err := temporal.Append(ctx, facts); err != nil {
		t.Fatalf("temporal.Append: %v", err)
	}
	if err := observations.Append(ctx, []domain.Observation{obs}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	if err := links.Append(ctx, []domain.FactLink{{
		ID:       "title-supported",
		Scope:    scope,
		Type:     domain.LinkSupports,
		From:     domain.GraphNodeRef{Kind: domain.GraphNodeObservationSpan, ID: "span-other-session"},
		To:       domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "title"},
		MergeKey: "supports:span-other-session:title",
	}}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "What book did Melanie read from Caroline's suggestion?", Limit: 10},
		Plan: &domain.QueryPlan{
			TotalCap:    10,
			TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskBridgeResolution},
			Intent: domain.QueryIntent{Features: domain.QueryFeatures{
				Proper: map[string]struct{}{"melanie": {}, "caroline": {}},
			}},
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
	got := detail.(diagnostic.LinkExpansionDetail)
	if got.AddedFacts != 0 || len(state.MergedItems) != 1 {
		t.Fatalf("adjacent bridge should require same session: detail=%+v items=%+v", got, state.MergedItems)
	}
	if got.AdjacentBridgeMatchedObservations != 0 || got.AdjacentBridgeAddedFacts != 0 {
		t.Fatalf("same-session rejection should be visible in bridge diagnostics: %+v", got)
	}
}
