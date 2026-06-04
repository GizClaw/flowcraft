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

func TestLinkExpansionMarksExistingSupportedTargetDirectionally(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	temporal := temporalstore.NewMemoryStore()
	observations := observationstore.New()
	links := linkstore.New()
	facts := []domain.TemporalFact{
		{ID: "seed", Scope: scope, Kind: domain.KindNote, Content: "Alice favorite drink is tea."},
		{ID: "neighbor", Scope: scope, Kind: domain.KindNote, Content: "ZXQ-774 calibration capsule note."},
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
		Plan:  &domain.QueryPlan{TotalCap: 10},
		MergedItems: []domain.ContextItem{
			{Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "seed", Scope: scope, Source: "retrieval", Score: 0.9}, Fact: facts[0]},
			{Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "neighbor", Scope: scope, Source: "retrieval", Score: 0.8}, Fact: facts[1]},
		},
	}

	if _, err := NewLinkExpansion(temporal, observations, links).Run(ctx, state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.MergedItems[0].Link.Type != "" {
		t.Fatalf("supports link should not be applied backward to seed: %+v", state.MergedItems[0])
	}
	if state.MergedItems[1].Link.Type != domain.LinkSupports {
		t.Fatalf("supports link should be applied to existing target: %+v", state.MergedItems[1])
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

func TestLinkExpansionAddsTypedSiblingWithoutTokenOverlapGate(t *testing.T) {
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
	if got.AddedFacts != 1 || len(state.MergedItems) != 2 {
		t.Fatalf("typed support link should nominate sibling for assessment: detail=%+v items=%+v", got, state.MergedItems)
	}
	if state.MergedItems[1].Fact.ID != "sibling" || state.MergedItems[1].Link.Type != domain.LinkSupports {
		t.Fatalf("added sibling should carry typed link provenance, got %+v", state.MergedItems[1])
	}
}

func TestLinkExpansionBoundsProcessedLinksAndEvidenceRefs(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	temporal := temporalstore.NewMemoryStore()
	observations := observationstore.New()
	links := linkstore.New()
	seed := domain.TemporalFact{ID: "seed", Scope: scope, Kind: domain.KindNote, Content: "Alice likes tea."}
	if err := temporal.Append(ctx, []domain.TemporalFact{seed}); err != nil {
		t.Fatalf("temporal.Append: %v", err)
	}
	var factLinks []domain.FactLink
	for i := 0; i < linkExpansionMaxProcessedLinks(&read.ReadState{Plan: &domain.QueryPlan{TotalCap: 2}})+10; i++ {
		obsID := "obs-limit-" + string(rune('a'+i%26)) + "-" + string(rune('a'+(i/26)%26))
		if err := observations.Append(ctx, []domain.Observation{{
			ID:    obsID,
			Scope: scope,
			Text:  "observation " + obsID,
		}}); err != nil {
			t.Fatalf("observations.Append: %v", err)
		}
		factLinks = append(factLinks, domain.FactLink{
			ID:    "link-" + obsID,
			Scope: scope,
			Type:  domain.LinkDerivedFrom,
			From:  domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "seed"},
			To:    domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: obsID},
		})
	}
	if err := links.Append(ctx, factLinks); err != nil {
		t.Fatalf("links.Append: %v", err)
	}
	state := &read.ReadState{
		Scope: scope,
		Query: domain.Query{Text: "Alice tea", Limit: 10},
		Plan:  &domain.QueryPlan{TotalCap: 2},
		MergedItems: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "seed", Scope: scope, Source: "retrieval", Score: 0.9},
			Fact:      seed,
		}},
	}

	detail, err := NewLinkExpansion(temporal, observations, links).Run(ctx, state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := detail.(diagnostic.LinkExpansionDetail)
	if got.ScannedLinks > linkExpansionMaxProcessedLinks(state) {
		t.Fatalf("processed links should be bounded, got %d > %d", got.ScannedLinks, linkExpansionMaxProcessedLinks(state))
	}
	if got.AddedEvidenceRefs > linkExpansionMaxEvidenceRefs(state) {
		t.Fatalf("added evidence refs should be bounded, got %d > %d", got.AddedEvidenceRefs, linkExpansionMaxEvidenceRefs(state))
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
	if got.AddedFacts != 2 || len(state.MergedItems) != 3 {
		t.Fatalf("AddedFacts=%d items=%+v", got.AddedFacts, state.MergedItems)
	}
	if state.MergedItems[1].Fact.ID != "sibling" {
		t.Fatalf("added fact = %q, want sibling", state.MergedItems[1].Fact.ID)
	}
	if state.MergedItems[2].Fact.ID != "pollution" {
		t.Fatalf("typed support discovery should nominate pollution for later assessment, got %q", state.MergedItems[2].Fact.ID)
	}
}
