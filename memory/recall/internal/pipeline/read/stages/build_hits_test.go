package stages

import (
	"context"
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
