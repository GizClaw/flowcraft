package recall

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestSave_CommitsObservationAssertionLinks(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	mem, err := New(
		WithObservationStore(observations),
		WithLinkStore(links),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	at := time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC)
	res, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "Alice likes tea",
			EvidenceRefs: []EvidenceRef{{
				ID:        "ev-1",
				MessageID: "msg-1",
				Role:      "user",
				Text:      "Alice likes tea",
				Timestamp: at,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("FactIDs = %v, want one fact", res.FactIDs)
	}

	gotObservations, err := observations.List(ctx, scope, ObservationListQuery{})
	if err != nil {
		t.Fatalf("observations.List: %v", err)
	}
	if len(gotObservations) != 1 {
		t.Fatalf("observations = %+v, want one observation", gotObservations)
	}
	if gotObservations[0].Kind != ObservationKindEvidence || gotObservations[0].Text != "Alice likes tea" {
		t.Fatalf("observation = %+v, want evidence text", gotObservations[0])
	}
	if len(gotObservations[0].Spans) != 1 {
		t.Fatalf("observation spans = %+v, want one span", gotObservations[0].Spans)
	}
	spanID := gotObservations[0].Spans[0].ID

	gotLinks, err := links.List(ctx, scope, LinkListQuery{})
	if err != nil {
		t.Fatalf("links.List: %v", err)
	}
	if !hasLink(gotLinks, LinkDerivedFrom, GraphNodeAssertion, res.FactIDs[0], GraphNodeObservationSpan, spanID) {
		t.Fatalf("missing derived_from link in %+v", gotLinks)
	}
	if !hasLink(gotLinks, LinkSupports, GraphNodeObservationSpan, spanID, GraphNodeAssertion, res.FactIDs[0]) {
		t.Fatalf("missing supports link in %+v", gotLinks)
	}
}

func TestSave_CommitsTurnObservationWhenNoAssertionsExtracted(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	mem, err := New(
		WithObservationStore(observations),
		WithLinkStore(links),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	res, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{
			ID:      "turn-miss",
			Role:    "user",
			Speaker: "Alice",
			Text:    "The extractor-only phrase is orbital marmalade.",
			Time:    time.Date(2026, 5, 31, 11, 0, 0, 0, time.UTC),
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("FactIDs = %v, want no assertions", res.FactIDs)
	}

	got, err := observations.List(ctx, scope, ObservationListQuery{})
	if err != nil {
		t.Fatalf("observations.List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("observations = %+v, want one turn observation", got)
	}
	if got[0].Kind != ObservationKindTurn || got[0].Text != "The extractor-only phrase is orbital marmalade." {
		t.Fatalf("observation = %+v, want raw turn", got[0])
	}
}

func TestSave_CommitsTurnObservationWhenIngestFails(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	mem, err := New(
		WithObservationStore(observations),
		WithLinkStore(links),
		withCompiler(failingGraphIngestor{}),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, err = mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{
			ID:      "turn-fail",
			Role:    "user",
			Speaker: "Alice",
			Text:    "The extractor parse failure should not erase this turn.",
		}},
	})
	if !errors.Is(err, errGraphIngestFailed) {
		t.Fatalf("save err = %v, want %v", err, errGraphIngestFailed)
	}

	got, err := observations.List(ctx, scope, ObservationListQuery{})
	if err != nil {
		t.Fatalf("observations.List: %v", err)
	}
	if len(got) != 1 || got[0].Text != "The extractor parse failure should not erase this turn." {
		t.Fatalf("observations = %+v, want committed raw turn", got)
	}
}

func TestSave_CommitsSupersedesLink(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	links := NewInMemoryLinkStore()
	mem, err := New(WithLinkStore(links))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	first, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactState,
			Subject:   "alice",
			Predicate: "city",
			Content:   "Paris",
		}},
	})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	second, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactState,
			Subject:   "alice",
			Predicate: "city",
			Content:   "Berlin",
		}},
	})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}

	gotLinks, err := links.List(ctx, scope, LinkListQuery{Types: []FactLinkType{LinkSupersedes}})
	if err != nil {
		t.Fatalf("links.List: %v", err)
	}
	if !hasLink(gotLinks, LinkSupersedes, GraphNodeAssertion, second.FactIDs[0], GraphNodeAssertion, first.FactIDs[0]) {
		t.Fatalf("missing supersedes link in %+v", gotLinks)
	}
}

var errGraphIngestFailed = errors.New("graph ingest failed")

type failingGraphIngestor struct{}

func (failingGraphIngestor) Compile(context.Context, port.IngestInput) (port.IngestResult, error) {
	return port.IngestResult{}, errGraphIngestFailed
}

func TestRecall_ObservationLaneReturnsRawEvidenceWhenExtractorMisses(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	mem, err := New(
		WithObservationStore(observations),
		WithLinkStore(links),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	rawText := "The extractor-only phrase is orbital marmalade."
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{
			ID:      "turn-miss",
			Role:    "user",
			Speaker: "Alice",
			Text:    rawText,
		}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	hits, trace, err := mem.(RecallExplainer).RecallExplain(ctx, scope, Query{Text: "orbital marmalade", Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 || hits[0].Observation.Text != rawText {
		t.Fatalf("hits = %+v, want raw observation evidence", hits)
	}
	if hits[0].Ref.Kind != GraphNodeObservation {
		t.Fatalf("hit ref = %+v, want observation", hits[0].Ref)
	}
	if !hasSource(hits[0], "observation") {
		t.Fatalf("hit sources = %+v, want observation source", hits[0].Sources)
	}
	_ = trace
}

func TestRecall_ObservationLaneReturnsRawEvidenceAfterIngestFailure(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	mem, err := New(withCompiler(failingGraphIngestor{}))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	rawText := "The extractor failure fallback phrase is blue nebula ledger."
	_, err = mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{
			ID:      "turn-fail",
			Role:    "user",
			Speaker: "Alice",
			Text:    rawText,
		}},
	})
	if !errors.Is(err, errGraphIngestFailed) {
		t.Fatalf("save err = %v, want %v", err, errGraphIngestFailed)
	}

	hits, trace, err := mem.(RecallExplainer).RecallExplain(ctx, scope, Query{Text: "blue nebula ledger", Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 || hits[0].Observation.Text != rawText {
		t.Fatalf("hits = %+v, want raw observation evidence after ingest failure", hits)
	}
	if !hasSource(hits[0], "observation") {
		t.Fatalf("hit sources = %+v, want observation source", hits[0].Sources)
	}
	_ = trace
}

func TestRecall_LinkExpansionAddsObservationEvidence(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	mem, err := New(
		WithObservationStore(observations),
		WithLinkStore(links),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	saved, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "Alice favorite drink is tea",
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	drainSideEffectsForTest(t, mem, scope)

	obs := Observation{
		ID:         "obs-extra",
		Scope:      scope,
		Kind:       ObservationKindTurn,
		SourceID:   "turn-extra",
		MessageID:  "msg-extra",
		Role:       "user",
		Text:       "Alice said she chooses tea when she needs a calm drink.",
		ObservedAt: time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC),
	}
	if err := observations.Append(ctx, []Observation{obs}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	if err := links.Append(ctx, []FactLink{{
		ID:       "lnk-extra-evidence",
		Scope:    scope,
		Type:     LinkSupports,
		From:     GraphNodeRef{Kind: GraphNodeObservation, ID: obs.ID},
		To:       GraphNodeRef{Kind: GraphNodeAssertion, ID: saved.FactIDs[0]},
		MergeKey: "supports:obs-extra:" + saved.FactIDs[0],
	}}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}

	hits, err := mem.Recall(ctx, scope, Query{Text: "Alice favorite drink", Limit: 5})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	hit := findHit(hits, saved.FactIDs[0])
	if hit == nil {
		t.Fatalf("missing saved fact in hits: %+v", hits)
	}
	if !hitHasEvidenceText(*hit, obs.Text) {
		t.Fatalf("hit evidence = %+v, want linked observation text %q", hit.Evidence, obs.Text)
	}
}

func TestRecall_LinkExpansionAddsSupportedAssertion(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	links := NewInMemoryLinkStore()
	mem, err := New(WithLinkStore(links))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	seed, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "Alice favorite drink is tea",
		}},
	})
	if err != nil {
		t.Fatalf("seed save: %v", err)
	}
	supported, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "ZXQ-774 calibration capsule note.",
		}},
	})
	if err != nil {
		t.Fatalf("supported save: %v", err)
	}
	drainSideEffectsForTest(t, mem, scope)

	if err := links.Append(ctx, []FactLink{{
		ID:       "lnk-supported-assertion",
		Scope:    scope,
		Type:     LinkSupports,
		From:     GraphNodeRef{Kind: GraphNodeAssertion, ID: seed.FactIDs[0]},
		To:       GraphNodeRef{Kind: GraphNodeAssertion, ID: supported.FactIDs[0]},
		MergeKey: "supports:" + seed.FactIDs[0] + ":" + supported.FactIDs[0],
	}}); err != nil {
		t.Fatalf("links.Append: %v", err)
	}

	hits, trace, err := mem.(RecallExplainer).RecallExplain(ctx, scope, Query{Text: "Alice favorite drink", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if findHit(hits, supported.FactIDs[0]) == nil {
		t.Fatalf("missing linked supported assertion in hits: %+v", hits)
	}
	if !hasTraceStage(trace, "link_expansion") {
		t.Fatalf("trace missing link_expansion stage: %+v", trace.Stages)
	}
}

func TestForgetHard_CleansGraphLedgerForFact(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	mem, err := New(
		WithObservationStore(observations),
		WithLinkStore(links),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	saved, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "Alice likes tea",
			EvidenceRefs: []EvidenceRef{{
				ID:   "ev-forget",
				Text: "Alice likes tea",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := mem.Forget(ctx, scope, saved.FactIDs[0], ForgetHard); err != nil {
		t.Fatalf("forget: %v", err)
	}

	gotLinks, err := links.List(ctx, scope, LinkListQuery{})
	if err != nil {
		t.Fatalf("links.List: %v", err)
	}
	if len(gotLinks) != 0 {
		t.Fatalf("links after forget = %+v, want empty", gotLinks)
	}
	gotObservations, err := observations.List(ctx, scope, ObservationListQuery{})
	if err != nil {
		t.Fatalf("observations.List: %v", err)
	}
	if len(gotObservations) != 0 {
		t.Fatalf("observations after forget = %+v, want empty", gotObservations)
	}
}

func TestRebuildAll_RehydratesGraphLedger(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	mem, err := New(
		WithObservationStore(observations),
		WithLinkStore(links),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	saved, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "Alice likes tea",
			EvidenceRefs: []EvidenceRef{{
				ID:   "ev-rebuild",
				Text: "Alice likes tea",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := links.DeleteByScope(ctx, scope); err != nil {
		t.Fatalf("links.DeleteByScope: %v", err)
	}
	if _, err := observations.DeleteByScope(ctx, scope); err != nil {
		t.Fatalf("observations.DeleteByScope: %v", err)
	}

	if err := mem.(ProjectionRebuilder).RebuildAll(ctx, scope); err != nil {
		t.Fatalf("rebuild all: %v", err)
	}

	gotObservations, err := observations.List(ctx, scope, ObservationListQuery{})
	if err != nil {
		t.Fatalf("observations.List: %v", err)
	}
	if len(gotObservations) != 1 {
		t.Fatalf("observations = %+v, want one rebuilt observation", gotObservations)
	}
	if len(gotObservations[0].Spans) != 1 {
		t.Fatalf("rebuilt observation spans = %+v, want one span", gotObservations[0].Spans)
	}
	spanID := gotObservations[0].Spans[0].ID
	gotLinks, err := links.List(ctx, scope, LinkListQuery{})
	if err != nil {
		t.Fatalf("links.List: %v", err)
	}
	if !hasLink(gotLinks, LinkDerivedFrom, GraphNodeAssertion, saved.FactIDs[0], GraphNodeObservationSpan, spanID) {
		t.Fatalf("missing rebuilt derived_from link in %+v", gotLinks)
	}
	if !hasLink(gotLinks, LinkSupports, GraphNodeObservationSpan, spanID, GraphNodeAssertion, saved.FactIDs[0]) {
		t.Fatalf("missing rebuilt supports link in %+v", gotLinks)
	}
}

func hasLink(links []FactLink, typ FactLinkType, fromKind GraphNodeKind, fromID string, toKind GraphNodeKind, toID string) bool {
	for _, link := range links {
		if link.Type != typ {
			continue
		}
		if link.From.Kind == fromKind && link.From.ID == fromID && link.To.Kind == toKind && link.To.ID == toID {
			return true
		}
	}
	return false
}

func findHit(hits []Hit, factID string) *Hit {
	for i := range hits {
		if hits[i].Fact.ID == factID {
			return &hits[i]
		}
	}
	return nil
}

func hitHasEvidenceText(hit Hit, text string) bool {
	for _, ref := range hit.Evidence {
		if ref.Text == text {
			return true
		}
	}
	return false
}

func hasSource(hit Hit, source string) bool {
	for _, got := range hit.Sources {
		if got == source {
			return true
		}
	}
	return false
}

func hasTraceStage(trace RecallTrace, stage string) bool {
	for _, st := range trace.Stages {
		if st.Stage == stage {
			return true
		}
	}
	return false
}
