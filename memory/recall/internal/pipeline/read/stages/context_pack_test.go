package stages

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

type reorderReranker struct{}

func (reorderReranker) Rerank(_ context.Context, _ string, hits []domain.Hit) ([]domain.Hit, error) {
	if len(hits) < 2 {
		return hits, nil
	}
	return []domain.Hit{hits[1], hits[0]}, nil
}

type inspectEvidenceReranker struct {
	counts []int
}

func (r *inspectEvidenceReranker) Rerank(_ context.Context, _ string, hits []domain.Hit) ([]domain.Hit, error) {
	r.counts = r.counts[:0]
	for _, hit := range hits {
		r.counts = append(r.counts, len(hit.Evidence))
	}
	return hits, nil
}

type cancelReranker struct{}

func (cancelReranker) Rerank(_ context.Context, _ string, hits []domain.Hit) ([]domain.Hit, error) {
	return hits, context.Canceled
}

func TestContextPackSnapshotsInputRerankedAndFinal(t *testing.T) {
	stage := NewContextPack(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "where did alice go"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "evidence", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "evidence", EvidenceRefs: []domain.EvidenceRef{{ID: "e1"}}},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "selected evidence"}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "distractor", Source: "entity", Score: 0.8, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "distractor", EvidenceRefs: []domain.EvidenceRef{{ID: "e2"}}},
				Evidence:  []domain.EvidenceRef{{ID: "e2", Text: "selected distractor"}},
			},
		},
	}
	state.EnsureTrace()

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.ContextPackDetail)
	if got.Input == nil || len(*got.Input) != 2 || (*got.Input)[0].FactID != "evidence" {
		t.Fatalf("input snapshots = %+v", got.Input)
	}
	if got.RerankedHits == nil || len(*got.RerankedHits) != 2 || (*got.RerankedHits)[0].FactID != "distractor" {
		t.Fatalf("reranked snapshots = %+v", got.RerankedHits)
	}
	if got.Hits == nil || len(*got.Hits) != 1 || (*got.Hits)[0].FactID != "evidence" {
		t.Fatalf("final snapshots = %+v", got.Hits)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "evidence" {
		t.Fatalf("state hits = %+v", state.Hits)
	}
	if len(state.Hits[0].Evidence) != 1 || state.Hits[0].Evidence[0].Text != "selected evidence" {
		t.Fatalf("hit evidence should survive context_pack/rerank: %+v", state.Hits[0].Evidence)
	}
}

func TestBuildGroundedHitsDoesNotAffectRerankerInput(t *testing.T) {
	reranker := &inspectEvidenceReranker{}
	stage := NewContextPack(reranker)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "Where did Avery move from?"},
		Ranked: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "move", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
			Fact: domain.TemporalFact{
				ID:      "move",
				Kind:    domain.KindState,
				Content: "Avery moved from her home country.",
				EvidenceRefs: []domain.EvidenceRef{
					{ID: "e1", Text: "Avery moved from her home country four years ago."},
					{ID: "e2", Text: "Avery said Sweden is where she moved from."},
				},
			},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "Avery moved from her home country four years ago."}},
		}},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(reranker.counts) != 1 || reranker.counts[0] != 1 {
		t.Fatalf("reranker should receive only candidate evidence, got counts %+v", reranker.counts)
	}
	if _, err := NewBuildGroundedHits().Run(context.Background(), state); err != nil {
		t.Fatalf("grounding Run returned error: %v", err)
	}
	if ids := evidenceIDs(state.Hits[0].Evidence); len(ids) != 2 || ids[0] != "e1" || ids[1] != "e2" {
		t.Fatalf("final output should still include supporting evidence, got %+v", ids)
	}
}

func TestContextPackSkipsSnapshotsWithoutTrace(t *testing.T) {
	stage := NewContextPack(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "where did alice go"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "evidence", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "evidence", EvidenceRefs: []domain.EvidenceRef{{ID: "e1"}}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "distractor", Source: "entity", Score: 0.8, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "distractor", EvidenceRefs: []domain.EvidenceRef{{ID: "e2"}}},
			},
		},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.ContextPackDetail)
	if got.Input != nil || got.RerankedHits != nil || got.Hits != nil {
		t.Fatalf("snapshots should be nil on nil-trace path: %+v", got)
	}
	if got.InputCount != 2 || got.Reranked != 2 || got.Count != 1 {
		t.Fatalf("counts should still be populated: %+v", got)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "evidence" {
		t.Fatalf("state hits = %+v", state.Hits)
	}
}

func TestContextPackKeepsHighestScoreContextForSharedEvidence(t *testing.T) {
	stage := NewContextPack(nil)
	shared := domain.EvidenceRef{ID: "e1", Text: "I painted that lake sunrise last year!"}
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "When did Jordan paint a sunrise?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "state", Source: "graph", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "state", Kind: domain.KindState, Content: "Jordan's painting of the lake sunrise is special to her.", EvidenceRefs: []domain.EvidenceRef{shared}},
				Evidence:  []domain.EvidenceRef{shared},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "event", Source: "retrieval", Score: 0.8, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "event", Kind: domain.KindEvent, Content: "Jordan painted a lake sunrise last year.", EvidenceRefs: []domain.EvidenceRef{shared}},
				Evidence:  []domain.EvidenceRef{shared},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "state" {
		t.Fatalf("shared evidence representative should keep the highest-score context, got %+v", state.Hits)
	}
}

func TestContextPackPropagatesRerankerContextCancellation(t *testing.T) {
	stage := NewContextPack(cancelReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "What did Alice buy?"},
		Ranked: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "a", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
			Fact:      domain.TemporalFact{ID: "a", Kind: domain.KindEvent, Content: "Alice bought woodworking."},
			Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice bought woodworking."}},
		}},
	}

	_, err := stage.Run(context.Background(), state)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context cancellation must propagate, got %v", err)
	}
}

func TestContextPackDedupesSameEvidence(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "What did Alice buy?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "a", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "a", Kind: domain.KindEvent, Content: "Alice bought woodworking."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice bought woodworking."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "b", Source: "graph", Score: 0.8, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "b", Kind: domain.KindEvent, Content: "Alice purchased woodworking."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice bought woodworking."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "c", Source: "retrieval", Score: 0.7, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "c", Kind: domain.KindState, Content: "Alice likes Paris."},
				Evidence:  []domain.EvidenceRef{{ID: "e2", Text: "Alice likes Paris."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 2 {
		t.Fatalf("want two final hits after dedupe/fill, got %+v", state.Hits)
	}
	if state.Hits[0].Fact.ID != "a" || state.Hits[1].Fact.ID != "c" {
		t.Fatalf("same evidence id should be deduped while preserving cap, got %+v", state.Hits)
	}
}

func TestContextPackSharedEvidenceRepresentativeDoesNotUseQuerySurface(t *testing.T) {
	input := newContextPackInput(3)
	hits := []domain.Hit{
		{
			Fact:     domain.TemporalFact{ID: "old", Kind: domain.KindState, Content: "Bob visited Paris."},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "Bob visited Paris."}},
			Score:    0.9,
		},
		{
			Fact:     domain.TemporalFact{ID: "new", Kind: domain.KindEvent, Content: "Alice bought woodworking."},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "Alice bought woodworking."}},
			Score:    0.8,
		},
		{
			Fact:     domain.TemporalFact{ID: "old", Kind: domain.KindState, Content: "Alice bought clay."},
			Evidence: []domain.EvidenceRef{{ID: "e2", Text: "Alice bought clay."}},
			Score:    0.7,
		},
	}

	candidates := contextPackCandidates(input, hits)
	got := map[string]bool{}
	for _, cand := range candidates {
		got[cand.hit.Fact.ID] = true
	}
	if got["new"] || !got["old"] {
		t.Fatalf("shared-evidence representative should preserve highest-score fact without query-surface replacement, got %+v", candidates)
	}
}

func TestContextPackKeepsSourceDiversity(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 3},
		Query: domain.Query{Text: "What did Alice say about woodworking class?"},
		Ranked: []domain.ContextItem{
			contextItemWithSource("retrieval-1", "e1", "retrieval", 0.90, "Alice discussed woodworking class logistics."),
			contextItemWithSource("retrieval-2", "e2", "retrieval", 0.88, "Alice discussed woodworking class timing."),
			contextItemWithSource("retrieval-3", "e3", "retrieval", 0.86, "Alice discussed woodworking class supplies."),
		},
		AfterTrust: []domain.ContextItem{
			contextItemWithSource("retrieval-1", "e1", "retrieval", 0.90, "Alice discussed woodworking class logistics."),
			contextItemWithSource("retrieval-2", "e2", "retrieval", 0.88, "Alice discussed woodworking class timing."),
			contextItemWithSource("retrieval-3", "e3", "retrieval", 0.86, "Alice discussed woodworking class supplies."),
			contextItemWithSource("graph-1", "e4", "graph", 0.84, "Alice discussed woodworking class project details."),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	gotGraph := false
	for _, hit := range state.Hits {
		for _, source := range hit.Sources {
			if source == "graph" {
				gotGraph = true
			}
		}
	}
	if !gotGraph {
		t.Fatalf("context packer should keep a similarly relevant alternate source, got %+v", state.Hits)
	}
}

func TestContextPackDoesNotPromoteWeakObservationAnchor(t *testing.T) {
	hits := []domain.Hit{
		{
			Fact:     domain.TemporalFact{ID: "answer", Kind: domain.KindState, Content: "Alice bought ceramic figurines."},
			Score:    0.90,
			Sources:  []string{"retrieval"},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "Alice bought ceramic figurines."}},
		},
		{
			Ref: domain.CandidateRef{Kind: domain.GraphNodeObservation, ID: "obs"},
			Observation: domain.Observation{
				ID:   "obs",
				Text: "Bob visited Paris.",
			},
			Score:    0.80,
			Sources:  []string{"observation"},
			Evidence: []domain.EvidenceRef{{ID: "obs", ObservationID: "obs", Text: "Bob visited Paris."}},
		},
		{
			Fact:     domain.TemporalFact{ID: "support", Kind: domain.KindState, Content: "Alice also likes pottery."},
			Score:    0.70,
			Sources:  []string{"graph"},
			Evidence: []domain.EvidenceRef{{ID: "e2", Text: "Alice also likes pottery."}},
		},
	}

	got := packRecallContextWithFeaturesAndDetail(hits, hits, 2)
	if len(got) == 0 || got[0].Ref.Kind == domain.GraphNodeObservation {
		t.Fatalf("weak observation evidence should not be promoted as primary anchor, got %+v", got)
	}
}

func TestBuildGroundedHitsGroundsSelectedEvidenceWithRelevantFactRefs(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "Where did Avery move from?"},
		Ranked: []domain.ContextItem{{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "move", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
			Fact: domain.TemporalFact{
				ID:      "move",
				Kind:    domain.KindState,
				Content: "Avery moved from her home country.",
				EvidenceRefs: []domain.EvidenceRef{
					{ID: "e1", Text: "Avery moved from her home country four years ago."},
					{ID: "e2", Text: "Avery said Sweden is where she moved from."},
					{ID: "e3", Text: "Jordan likes woodworking classes."},
				},
			},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "Avery moved from her home country four years ago."}},
		}},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, err := NewBuildGroundedHits().Run(context.Background(), state); err != nil {
		t.Fatalf("grounding Run returned error: %v", err)
	}
	if len(state.Hits) != 1 {
		t.Fatalf("hits = %+v", state.Hits)
	}
	got := evidenceIDs(state.Hits[0].Evidence)
	want := []string{"e1", "e2", "e3"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("grounding evidence ids = %+v, want %+v", got, want)
	}
}

func TestBuildGroundedHitsEvidenceIsCapped(t *testing.T) {
	refs := []domain.EvidenceRef{
		{ID: "e1", Text: "Alice bought woodworking."},
		{ID: "e2", Text: "Alice bought ceramic figurines."},
		{ID: "e3", Text: "Alice bought a violin."},
		{ID: "e4", Text: "Alice bought a book."},
	}
	got := selectGroundingEvidence("What did Alice buy?", []domain.EvidenceRef{refs[0]}, refs)
	if len(got) != maxHitEvidenceRefs {
		t.Fatalf("grounding evidence count = %d, want %d: %+v", len(got), maxHitEvidenceRefs, got)
	}
	if got[0].ID != "e1" {
		t.Fatalf("selected evidence should stay first, got %+v", evidenceIDs(got))
	}
}

func TestBuildGroundedHitsAppendsSupportingRefsInSourceOrder(t *testing.T) {
	refs := []domain.EvidenceRef{
		{ID: "e1", Text: "Jordan has two cats named Oscar and Luna."},
		{ID: "e2", Text: "Jordan went hiking with her family."},
		{ID: "e3", Text: "The woodworking class was relaxing."},
	}
	got := selectGroundingEvidence("What pets does Jordan have?", []domain.EvidenceRef{refs[0]}, refs)
	if ids := evidenceIDs(got); len(ids) != 3 || ids[0] != "e1" || ids[1] != "e2" || ids[2] != "e3" {
		t.Fatalf("supporting refs should be appended in source order, got %+v", ids)
	}
}

func TestBuildGroundedHitsDoesNotPrioritizeTimestampedSupport(t *testing.T) {
	refs := []domain.EvidenceRef{
		{ID: "e1", Text: "Jordan painted a lake sunrise last year."},
		{ID: "e2", Text: "Jordan shared the sunrise painting with Avery.", Timestamp: time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC)},
		{ID: "e3", Text: "Avery discussed adoption paperwork."},
	}
	got := selectGroundingEvidence("When did Jordan paint a sunrise?", []domain.EvidenceRef{refs[0]}, refs)
	ids := evidenceIDs(got)
	if len(ids) != 3 || ids[0] != "e1" || ids[1] != "e2" || ids[2] != "e3" {
		t.Fatalf("supporting refs should stay in source order without temporal prioritization, got %+v", ids)
	}
}

func BenchmarkPackRecallContext(b *testing.B) {
	ordered := make([]domain.Hit, 0, 30)
	pool := make([]domain.Hit, 0, 120)
	for i := 0; i < 120; i++ {
		id := "hit-" + strconv.Itoa(i)
		evidenceID := "e-" + strconv.Itoa(i)
		marker := "marker" + strconv.Itoa(i)
		text := "Bob visited Paris and Carol likes hiking near " + marker + "."
		content := text
		score := 1.0 - float64(i)*0.005
		if i%17 == 0 {
			text = "On 2023-05-07 Alice bought 2 ceramic figurines and later played the violin."
			content = "Alice bought 2 ceramic figurines and plays the violin."
			score = 0.15
		}
		hit := domain.Hit{
			Fact: domain.TemporalFact{
				ID:      id,
				Kind:    domain.KindEvent,
				Content: content,
				Subject: "Alice",
			},
			Evidence: []domain.EvidenceRef{{ID: evidenceID, Text: text}},
			Score:    score,
			Sources:  []string{"retrieval"},
		}
		pool = append(pool, hit)
		if i < 30 {
			ordered = append(ordered, hit)
		}
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hits := packRecallContextWithFeaturesAndDetail(ordered, pool, 30)
		if len(hits) != 30 {
			b.Fatalf("len(hits) = %d, want 30", len(hits))
		}
	}
}

func evidenceIDs(refs []domain.EvidenceRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.ID)
	}
	return out
}

func contextItemWithSource(id, evidenceID, source string, score float64, text string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: id, Source: source, Score: score, EvidenceIDs: []string{evidenceID}},
		Fact:      domain.TemporalFact{ID: id, Kind: domain.KindState, Content: text},
		Evidence:  []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}
