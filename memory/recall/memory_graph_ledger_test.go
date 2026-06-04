package recall

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
)

func TestSave_ParameterFailsBeforeAppendWhenGraphDependenciesMissing(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = mem.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{{
		Kind:      FactParameter,
		Content:   "experiment.temperature = 0.2",
		Subject:   "experiment",
		Predicate: "parameter_value",
		Object:    "0.2",
		Metadata: map[string]any{
			MetaParameterOwner:           "experiment",
			MetaParameterCanonicalName:   "temperature",
			MetaParameterNormalizedValue: "0.2",
		},
		EvidenceRefs: []EvidenceRef{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			Text:          "temperature = 0.2",
		}},
	}}})
	if err == nil {
		t.Fatal("Save err = nil, want graph dependency validation error")
	}
	got, listErr := store.List(ctx, scope, port.ListQuery{})
	if listErr != nil {
		t.Fatalf("store.List: %v", listErr)
	}
	if len(got) != 0 {
		t.Fatalf("stored facts = %+v, want none", got)
	}
}

func TestSave_ParameterMissingObservationFailsBeforeAppend(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		WithTemporalStore(store),
		WithObservationStore(NewInMemoryObservationStore()),
		WithLinkStore(NewInMemoryLinkStore()),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = mem.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{{
		Kind:      FactParameter,
		Content:   "experiment.temperature = 0.2",
		Subject:   "experiment",
		Predicate: "parameter_value",
		Object:    "0.2",
		Metadata: map[string]any{
			MetaParameterOwner:           "experiment",
			MetaParameterCanonicalName:   "temperature",
			MetaParameterNormalizedValue: "0.2",
		},
		EvidenceRefs: []EvidenceRef{{
			ObservationID: "missing-obs",
			SpanID:        "span-1",
			Text:          "temperature = 0.2",
		}},
	}}})
	if err == nil {
		t.Fatal("Save err = nil, want missing graph dependency error")
	}
	got, listErr := store.List(ctx, scope, port.ListQuery{})
	if listErr != nil {
		t.Fatalf("store.List: %v", listErr)
	}
	if len(got) != 0 {
		t.Fatalf("stored facts = %+v, want none", got)
	}
}

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
	raw := Observation{
		ID:         "obs-tea",
		Scope:      scope,
		Kind:       ObservationKindTurn,
		SourceID:   "ev-1",
		MessageID:  "msg-1",
		Role:       "user",
		Text:       "Alice likes tea",
		ObservedAt: at,
		Spans: []ObservationSpan{{
			ID:            "span-tea",
			ObservationID: "obs-tea",
			SourceID:      "ev-1",
			Kind:          ObservationSpanKindTurn,
			Text:          "Alice likes tea",
			Start:         0,
			End:           len("Alice likes tea"),
		}},
	}
	if err := observations.Append(ctx, []Observation{raw}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "Alice likes tea",
			EvidenceRefs: []EvidenceRef{{
				ID:            "ev-1",
				MessageID:     "msg-1",
				ObservationID: raw.ID,
				SpanID:        raw.Spans[0].ID,
				Role:          "user",
				Text:          "Alice likes tea",
				Timestamp:     at,
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
	if gotObservations[0].Kind != ObservationKindTurn || gotObservations[0].Text != "Alice likes tea" {
		t.Fatalf("observation = %+v, want pre-existing raw turn", gotObservations[0])
	}
	if len(gotObservations[0].Spans) != 1 {
		t.Fatalf("observation spans = %+v, want one span", gotObservations[0].Spans)
	}
	spanID := raw.Spans[0].ID

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

	rawText := "The extractor-only phrase is Orbital Marmalade."
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

	hits, trace, err := mem.(RecallExplainer).RecallExplain(ctx, scope, Query{Text: "Orbital Marmalade", Limit: 5})
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
	mem, err := New(
		withCompiler(failingGraphIngestor{}),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	rawText := "The extractor failure fallback phrase is Blue Nebula Ledger."
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

	hits, trace, err := mem.(RecallExplainer).RecallExplain(ctx, scope, Query{Text: "Blue Nebula Ledger", Limit: 5})
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

func TestRecall_GroundedHitIncludesSupportedAssertionLink(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	links := NewInMemoryLinkStore()
	mem, err := New(
		WithObservationStore(NewInMemoryObservationStore()),
		WithLinkStore(links),
	)
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
			Kind:      FactNote,
			Subject:   "alice",
			Predicate: "favorite_drink",
			Content:   "ZXQ-774 calibration capsule note.",
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

	hits, err := mem.Recall(ctx, scope, Query{Text: "Alice favorite drink", Subject: "alice", Predicate: "favorite_drink", Limit: 10})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	hit := findHit(hits, seed.FactIDs[0])
	if hit == nil {
		t.Fatalf("missing seed assertion in hits: %+v", hits)
	}
	if !hasLink(hit.EvidencePacket.Links, LinkSupports, GraphNodeAssertion, seed.FactIDs[0], GraphNodeAssertion, supported.FactIDs[0]) {
		t.Fatalf("hit evidence packet links = %+v, want support link to %s", hit.EvidencePacket.Links, supported.FactIDs[0])
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

func TestForgetHard_StoreDeleteFailureRestoresGraphLedger(t *testing.T) {
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	baseStore := temporalstore.NewMemoryStore()
	store := &deleteFailTemporalStore{TemporalStore: baseStore, err: errors.New("delete unavailable")}
	observations := NewInMemoryObservationStore()
	links := NewInMemoryLinkStore()
	projection := &forgetObservationProjectionRecorder{}
	mem, err := New(
		WithTemporalStore(store),
		WithObservationStore(observations),
		WithLinkStore(links),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	m := mem.(*memory)
	m.observationProjection = projection
	raw := Observation{
		ID:        "obs-restore",
		Scope:     scope,
		Kind:      ObservationKindTurn,
		SourceID:  "ev-restore",
		MessageID: "msg-restore",
		Text:      "Alice likes tea",
		Spans: []ObservationSpan{{
			ID:            "span-restore",
			ObservationID: "obs-restore",
			SourceID:      "ev-restore",
			Kind:          ObservationSpanKindTurn,
			Text:          "Alice likes tea",
			Start:         0,
			End:           len("Alice likes tea"),
		}},
	}
	if err := observations.Append(ctx, []Observation{raw}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}

	saved, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "Alice shared that she likes tea",
			EvidenceRefs: []EvidenceRef{{
				ID:            "ev-restore",
				MessageID:     "msg-restore",
				ObservationID: raw.ID,
				SpanID:        raw.Spans[0].ID,
				Text:          "Alice likes tea",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	factID := saved.FactIDs[0]
	if got, err := links.List(ctx, scope, LinkListQuery{}); err != nil || len(got) == 0 {
		t.Fatalf("precondition links = %+v err=%v, want graph links", got, err)
	}
	if got, err := observations.List(ctx, scope, ObservationListQuery{}); err != nil || len(got) == 0 {
		t.Fatalf("precondition observations = %+v err=%v, want observations", got, err)
	}
	projection.projected = nil
	projection.forgotten = nil

	err = mem.Forget(ctx, scope, factID, ForgetHard)
	if err == nil {
		t.Fatal("Forget must return store delete error")
	}
	if _, err := baseStore.Get(ctx, scope, factID); err != nil {
		t.Fatalf("canonical fact should remain after delete failure: %v", err)
	}
	if got, err := links.List(ctx, scope, LinkListQuery{}); err != nil || len(got) == 0 {
		t.Fatalf("graph links should be restored after delete failure, got %+v err=%v", got, err)
	}
	if got, err := observations.List(ctx, scope, ObservationListQuery{}); err != nil || len(got) != 1 {
		t.Fatalf("raw observation should remain after delete failure, got %+v err=%v", got, err)
	}
	if len(projection.forgotten) == 0 {
		t.Fatalf("test precondition failed: observation projection was not cleaned")
	}
	if len(projection.projected) == 0 {
		t.Fatalf("observation projection should be restored after delete failure")
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
	raw := Observation{
		ID:       "obs-rebuild",
		Scope:    scope,
		Kind:     ObservationKindTurn,
		SourceID: "ev-rebuild",
		Text:     "Alice likes tea",
		Spans: []ObservationSpan{{
			ID:            "span-rebuild",
			ObservationID: "obs-rebuild",
			SourceID:      "ev-rebuild",
			Kind:          ObservationSpanKindTurn,
			Text:          "Alice likes tea",
			Start:         0,
			End:           len("Alice likes tea"),
		}},
	}
	if err := observations.Append(ctx, []Observation{raw}); err != nil {
		t.Fatalf("observations.Append: %v", err)
	}

	saved, err := mem.Save(ctx, scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactNote,
			Content: "Alice likes tea",
			EvidenceRefs: []EvidenceRef{{
				ID:            "ev-rebuild",
				ObservationID: raw.ID,
				SpanID:        raw.Spans[0].ID,
				Text:          "Alice likes tea",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := links.DeleteByScope(ctx, scope); err != nil {
		t.Fatalf("links.DeleteByScope: %v", err)
	}

	if err := mem.(ProjectionRebuilder).RebuildAll(ctx, scope); err != nil {
		t.Fatalf("rebuild all: %v", err)
	}

	gotObservations, err := observations.List(ctx, scope, ObservationListQuery{})
	if err != nil {
		t.Fatalf("observations.List: %v", err)
	}
	if len(gotObservations) != 1 {
		t.Fatalf("observations = %+v, want preserved raw observation", gotObservations)
	}
	if len(gotObservations[0].Spans) != 1 {
		t.Fatalf("raw observation spans = %+v, want one span", gotObservations[0].Spans)
	}
	spanID := raw.Spans[0].ID
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

func TestRebuildAll_PreservesRawTurnObservations(t *testing.T) {
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

	rawText := "The raw-only rebuild phrase is violet sundial."
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Turns: []TurnContext{{
			ID:   "turn-raw-rebuild",
			Role: "user",
			Text: rawText,
		}},
	}); err != nil {
		t.Fatalf("save raw turn: %v", err)
	}
	if err := mem.(ProjectionRebuilder).RebuildAll(ctx, scope); err != nil {
		t.Fatalf("rebuild all: %v", err)
	}

	got, err := observations.List(ctx, scope, ObservationListQuery{})
	if err != nil {
		t.Fatalf("observations.List: %v", err)
	}
	if len(got) != 1 || got[0].Kind != ObservationKindTurn || got[0].Text != rawText {
		t.Fatalf("observations after rebuild = %+v, want raw turn preserved", got)
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

type deleteFailTemporalStore struct {
	TemporalStore
	err error
}

func (s *deleteFailTemporalStore) Delete(context.Context, Scope, []string) error {
	return s.err
}

type forgetObservationProjectionRecorder struct {
	projected []Observation
	forgotten []string
}

func (p *forgetObservationProjectionRecorder) Name() string { return "observation" }

func (p *forgetObservationProjectionRecorder) ProjectObservations(_ context.Context, observations []Observation) error {
	p.projected = append(p.projected, observations...)
	return nil
}

func (p *forgetObservationProjectionRecorder) RebuildObservations(context.Context, Scope, []Observation) error {
	return nil
}

func (p *forgetObservationProjectionRecorder) ForgetObservations(_ context.Context, _ Scope, ids []string) error {
	p.forgotten = append(p.forgotten, ids...)
	return nil
}

func (p *forgetObservationProjectionRecorder) ClearObservationScope(context.Context, Scope) error {
	return nil
}
