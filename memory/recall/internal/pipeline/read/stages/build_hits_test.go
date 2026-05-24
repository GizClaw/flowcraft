package stages

import (
	"context"
	"strconv"
	"testing"

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

func TestBuildHitsSnapshotsInputRerankedAndFinal(t *testing.T) {
	stage := NewBuildHits(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "where did alice go"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "evidence", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "evidence", EvidenceRefs: []domain.EvidenceRef{{ID: "e1"}}},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "selected evidence"}},
			},
			{
				Candidate: domain.Candidate{FactID: "distractor", Source: "entity", Score: 0.8, EvidenceIDs: []string{"e2"}},
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
	got := detail.(diagnostic.BuildHitsDetail)
	if got.Input == nil || len(*got.Input) != 2 || (*got.Input)[0].FactID != "evidence" {
		t.Fatalf("input snapshots = %+v", got.Input)
	}
	if got.RerankedHits == nil || len(*got.RerankedHits) != 2 || (*got.RerankedHits)[0].FactID != "distractor" {
		t.Fatalf("reranked snapshots = %+v", got.RerankedHits)
	}
	if got.Hits == nil || len(*got.Hits) != 1 || (*got.Hits)[0].FactID != "distractor" {
		t.Fatalf("final snapshots = %+v", got.Hits)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "distractor" {
		t.Fatalf("state hits = %+v", state.Hits)
	}
	if len(state.Hits[0].Evidence) != 1 || state.Hits[0].Evidence[0].Text != "selected distractor" {
		t.Fatalf("hit evidence should survive build_hits/rerank: %+v", state.Hits[0].Evidence)
	}
}

func TestBuildHitsSkipsSnapshotsWithoutTrace(t *testing.T) {
	stage := NewBuildHits(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "where did alice go"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "evidence", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "evidence", EvidenceRefs: []domain.EvidenceRef{{ID: "e1"}}},
			},
			{
				Candidate: domain.Candidate{FactID: "distractor", Source: "entity", Score: 0.8, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "distractor", EvidenceRefs: []domain.EvidenceRef{{ID: "e2"}}},
			},
		},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.BuildHitsDetail)
	if got.Input != nil || got.RerankedHits != nil || got.Hits != nil {
		t.Fatalf("snapshots should be nil on nil-trace path: %+v", got)
	}
	if got.InputCount != 2 || got.Reranked != 2 || got.Count != 1 {
		t.Fatalf("counts should still be populated: %+v", got)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "distractor" {
		t.Fatalf("state hits = %+v", state.Hits)
	}
}

func TestBuildHitsEvidenceAwareRescueReplacesWeakFinalHit(t *testing.T) {
	stage := NewBuildHits(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "When did Alice buy 2 ceramic figurines?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "distractor", Source: "entity", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "distractor", Kind: domain.KindState, Content: "Alice likes Paris."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice likes Paris."}},
			},
		},
		AfterTrust: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "distractor", Source: "entity", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "distractor", Kind: domain.KindState, Content: "Alice likes Paris."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice likes Paris."}},
			},
			{
				Candidate: domain.Candidate{FactID: "specific", Source: "retrieval", Score: 0.2, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "specific", Kind: domain.KindEvent, Content: "Alice bought 2 ceramic figurines."},
				Evidence:  []domain.EvidenceRef{{ID: "e2", Text: "On 2023-05-07 Alice bought 2 ceramic figurines."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "specific" {
		t.Fatalf("evidence-aware rescue should keep the specific evidence hit, got %+v", state.Hits)
	}
}

func TestBuildHitsEvidenceAwareRescueCanReplaceSeveralWeakHits(t *testing.T) {
	stage := NewBuildHits(nil)
	query := "What books and instruments does Alice like?"
	weak := []domain.ContextItem{
		weakContextItem("weak-1", "e1", "Bob visited Paris."),
		weakContextItem("weak-2", "e2", "Carol likes hiking."),
		weakContextItem("weak-3", "e3", "Dylan cooked soup."),
		weakContextItem("weak-4", "e4", "Eve bought pottery."),
	}
	strong := []domain.ContextItem{
		strongContextItem("book", "e5", "Alice likes reading Charlotte's Web."),
		strongContextItem("violin", "e6", "Alice likes playing the violin."),
		strongContextItem("clarinet", "e7", "Alice likes playing the clarinet."),
		strongContextItem("guitar", "e8", "Alice likes playing the guitar."),
	}
	state := &read.ReadState{
		Plan:       &domain.QueryPlan{TotalCap: 4},
		Query:      domain.Query{Text: query},
		Ranked:     append([]domain.ContextItem(nil), weak...),
		AfterTrust: append(append([]domain.ContextItem(nil), weak...), strong...),
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, hit := range state.Hits {
		got[hit.Fact.ID] = true
	}
	for _, want := range []string{"book", "violin", "clarinet", "guitar"} {
		if !got[want] {
			t.Fatalf("expected rescued hit %q in final hits, got %+v", want, state.Hits)
		}
	}
}

func TestBuildHitsFinalSelectionHybridReranksFullPool(t *testing.T) {
	stage := NewBuildHits(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "When did Alice buy 2 ceramic figurines?"},
		Ranked: []domain.ContextItem{
			weakContextItem("weak-1", "e1", "Bob visited Paris."),
			weakContextItem("weak-2", "e2", "Carol likes hiking."),
		},
		AfterTrust: []domain.ContextItem{
			weakContextItem("weak-1", "e1", "Bob visited Paris."),
			weakContextItem("weak-2", "e2", "Carol likes hiking."),
			{
				Candidate: domain.Candidate{FactID: "specific", Source: "retrieval", Score: 0.15, EvidenceIDs: []string{"e3"}},
				Fact:      domain.TemporalFact{ID: "specific", Kind: domain.KindEvent, Content: "Alice bought 2 ceramic figurines."},
				Evidence:  []domain.EvidenceRef{{ID: "e3", Text: "On 2023-05-07 Alice bought 2 ceramic figurines."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, hit := range state.Hits {
		got[hit.Fact.ID] = true
	}
	if !got["specific"] {
		t.Fatalf("hybrid final selection should rerank strong evidence from the wider pool, got %+v", state.Hits)
	}
}

func TestBuildHitsFinalSelectionUsesFactContentWhenEvidenceIsThin(t *testing.T) {
	stage := NewBuildHits(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "What instrument does Alice play?"},
		Ranked: []domain.ContextItem{
			weakContextItem("weak", "e1", "Bob visited Paris."),
		},
		AfterTrust: []domain.ContextItem{
			weakContextItem("weak", "e1", "Bob visited Paris."),
			{
				Candidate: domain.Candidate{FactID: "instrument", Source: "graph", Score: 0.1, EvidenceIDs: []string{"e2"}},
				Fact: domain.TemporalFact{
					ID:        "instrument",
					Kind:      domain.KindPreference,
					Content:   "Alice plays the violin.",
					Subject:   "Alice",
					Predicate: "plays",
					Object:    "violin",
				},
				Evidence: []domain.EvidenceRef{{ID: "e2", Text: "She mentioned it."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "instrument" {
		t.Fatalf("hybrid final selection should score fact content as well as evidence text, got %+v", state.Hits)
	}
}

func TestBuildHitsFinalSelectionDedupesSameEvidence(t *testing.T) {
	stage := NewBuildHits(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "What did Alice buy?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "a", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "a", Kind: domain.KindEvent, Content: "Alice bought pottery."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice bought pottery."}},
			},
			{
				Candidate: domain.Candidate{FactID: "b", Source: "graph", Score: 0.8, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "b", Kind: domain.KindEvent, Content: "Alice purchased pottery."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice bought pottery."}},
			},
			{
				Candidate: domain.Candidate{FactID: "c", Source: "retrieval", Score: 0.7, EvidenceIDs: []string{"e2"}},
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

func TestBuildHitsGroundsSelectedEvidenceWithRelevantFactRefs(t *testing.T) {
	stage := NewBuildHits(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "Where did Caroline move from?"},
		Ranked: []domain.ContextItem{{
			Candidate: domain.Candidate{FactID: "move", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
			Fact: domain.TemporalFact{
				ID:      "move",
				Kind:    domain.KindState,
				Content: "Caroline moved from her home country.",
				EvidenceRefs: []domain.EvidenceRef{
					{ID: "e1", Text: "Caroline moved from her home country four years ago."},
					{ID: "e2", Text: "Caroline said Sweden is where she moved from."},
					{ID: "e3", Text: "Melanie likes pottery classes."},
				},
			},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "Caroline moved from her home country four years ago."}},
		}},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 {
		t.Fatalf("hits = %+v", state.Hits)
	}
	got := evidenceIDs(state.Hits[0].Evidence)
	want := []string{"e1", "e2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("grounding evidence ids = %+v, want %+v", got, want)
	}
}

func TestBuildHitsGroundingEvidenceIsCapped(t *testing.T) {
	refs := []domain.EvidenceRef{
		{ID: "e1", Text: "Alice bought pottery."},
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

func TestBuildHitsGroundingSkipsWeakStopwordOrEntityOnlyRefs(t *testing.T) {
	refs := []domain.EvidenceRef{
		{ID: "e1", Text: "Melanie has two cats named Oscar and Luna."},
		{ID: "e2", Text: "Melanie went hiking with her family."},
		{ID: "e3", Text: "The pottery class was relaxing."},
	}
	got := selectGroundingEvidence("What pets does Melanie have?", []domain.EvidenceRef{refs[0]}, refs)
	if ids := evidenceIDs(got); len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("weak entity-only refs should not be added, got %+v", ids)
	}
}

func BenchmarkSelectFinalHybridRerankHits(b *testing.B) {
	query := "When did Alice buy 2 ceramic figurines and which instrument does she play?"
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
		hits := selectFinalHybridRerankHits(query, ordered, pool, 30)
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

func weakContextItem(id, evidenceID, text string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{FactID: id, Source: "retrieval", Score: 0.9, EvidenceIDs: []string{evidenceID}},
		Fact:      domain.TemporalFact{ID: id, Kind: domain.KindState, Content: text},
		Evidence:  []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}

func strongContextItem(id, evidenceID, text string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{FactID: id, Source: "retrieval", Score: 0.2, EvidenceIDs: []string{evidenceID}},
		Fact:      domain.TemporalFact{ID: id, Kind: domain.KindState, Content: text},
		Evidence:  []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}
