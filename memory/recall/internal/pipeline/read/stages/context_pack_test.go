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
	linkstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/link"
	observationstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/observation"
)

type reorderReranker struct{}

func (reorderReranker) Rerank(_ context.Context, _ string, hits []domain.Hit) ([]domain.Hit, error) {
	if len(hits) < 2 {
		return hits, nil
	}
	return []domain.Hit{hits[1], hits[0]}, nil
}

type factIDOrderReranker struct {
	ids  []string
	only bool
}

func (r factIDOrderReranker) Rerank(_ context.Context, _ string, hits []domain.Hit) ([]domain.Hit, error) {
	byID := make(map[string]domain.Hit, len(hits))
	for _, hit := range hits {
		byID[hit.Fact.ID] = hit
	}
	out := make([]domain.Hit, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	for _, id := range r.ids {
		hit, ok := byID[id]
		if !ok {
			continue
		}
		out = append(out, hit)
		seen[id] = struct{}{}
	}
	if !r.only {
		for _, hit := range hits {
			if _, ok := seen[hit.Fact.ID]; ok {
				continue
			}
			out = append(out, hit)
		}
	}
	return out, nil
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

type injectingReranker struct {
	injected domain.Hit
}

func (r injectingReranker) Rerank(_ context.Context, _ string, hits []domain.Hit) ([]domain.Hit, error) {
	out := []domain.Hit{r.injected}
	out = append(out, hits...)
	return out, nil
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
	if got.Hits == nil || len(*got.Hits) != 1 || (*got.Hits)[0].FactID != "distractor" {
		t.Fatalf("final snapshots = %+v", got.Hits)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "distractor" {
		t.Fatalf("state hits = %+v", state.Hits)
	}
	if len(state.Hits[0].Evidence) != 1 || state.Hits[0].Evidence[0].Text != "selected distractor" {
		t.Fatalf("hit evidence should survive context_pack/rerank: %+v", state.Hits[0].Evidence)
	}
}

func TestContextPackLimitOneUsesRerankedTopHit(t *testing.T) {
	stage := NewContextPack(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "where is the capsule"},
		Ranked: []domain.ContextItem{
			contextItemWithSource("rank-top", "e1", "retrieval", 0.50, "The capsule status is ready."),
			contextItemWithSource("reranked-top", "e2", "retrieval", 0.40, "The capsule is in the blue box."),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "reranked-top" {
		t.Fatalf("limit=1 should keep reranker top hit, got %+v", hitFactIDs(state.Hits))
	}
}

func TestContextPackRerankedTopBeatsHigherScoredOriginalRankTop(t *testing.T) {
	stage := NewContextPack(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "where is the capsule"},
		Ranked: []domain.ContextItem{
			contextItemWithSource("high-score-rank-top", "e1", "retrieval", 0.99, "The capsule calibration is due."),
			contextItemWithSource("low-score-reranked-top", "e2", "retrieval", 0.01, "The capsule is in the blue box."),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "low-score-reranked-top" {
		t.Fatalf("higher original rank score should not override reranker top hit, got %+v", hitFactIDs(state.Hits))
	}
}

func TestContextPackRerankerControlsFinalTopKPrefix(t *testing.T) {
	cases := []struct {
		name string
		cap  int
		want []string
	}{
		{name: "topK 2", cap: 2, want: []string{"reranked-1", "reranked-2"}},
		{name: "topK 3", cap: 3, want: []string{"reranked-1", "reranked-2", "reranked-3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage := NewContextPack(factIDOrderReranker{ids: []string{
				"reranked-1",
				"reranked-2",
				"reranked-3",
				"original-high-1",
				"original-high-2",
			}})
			state := &read.ReadState{
				Plan:  &domain.QueryPlan{TotalCap: tc.cap},
				Query: domain.Query{Text: "where is the capsule"},
				Ranked: []domain.ContextItem{
					contextItemWithSource("original-high-1", "e1", "retrieval", 0.99, "The capsule calibration is due."),
					contextItemWithSource("original-high-2", "e2", "retrieval", 0.98, "The capsule maintenance log is nearby."),
					contextItemWithSource("reranked-1", "e3", "retrieval", 0.03, "The capsule is in the blue box."),
					contextItemWithSource("reranked-2", "e4", "retrieval", 0.02, "The blue box is under the desk."),
					contextItemWithSource("reranked-3", "e5", "retrieval", 0.01, "The desk is in the workshop."),
				},
			}

			if _, err := stage.Run(context.Background(), state); err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if got := hitFactIDs(state.Hits); !sameStringSlice(got, tc.want) {
				t.Fatalf("reranker prefix should control final topK, got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestContextPackFillsAfterShortRerankedPrefix(t *testing.T) {
	stage := NewContextPack(factIDOrderReranker{ids: []string{
		"reranked-1",
		"reranked-2",
	}, only: true})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 3},
		Query: domain.Query{Text: "where is the capsule"},
		Ranked: []domain.ContextItem{
			contextItemWithSource("original-high-1", "e1", "retrieval", 0.99, "The capsule calibration is due."),
			contextItemWithSource("original-high-2", "e2", "retrieval", 0.98, "The capsule maintenance log is nearby."),
			contextItemWithSource("reranked-1", "e3", "retrieval", 0.03, "The capsule is in the blue box."),
			contextItemWithSource("reranked-2", "e4", "retrieval", 0.02, "The blue box is under the desk."),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	want := []string{"reranked-1", "reranked-2", "original-high-1"}
	if got := hitFactIDs(state.Hits); !sameStringSlice(got, want) {
		t.Fatalf("short reranked prefix should be filled without reordering it, got %+v want %+v", got, want)
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

func TestContextPackFiltersRerankerInjectedUnassessedHit(t *testing.T) {
	injected := domain.Hit{
		Ref:      domain.CandidateRef{Kind: domain.GraphNodeAssertion, ID: "unassessed"},
		Fact:     domain.TemporalFact{ID: "unassessed", Kind: domain.KindState, Content: "This candidate did not pass assessment."},
		Evidence: []domain.EvidenceRef{{ID: "e2", Text: "unassessed evidence"}},
		Score:    0.99,
	}
	stage := NewContextPack(injectingReranker{injected: injected})
	state := &read.ReadState{
		Plan:              &domain.QueryPlan{TotalCap: 1},
		Query:             domain.Query{Text: "ZXQ capsule"},
		AssessmentApplied: true,
		Ranked: []domain.ContextItem{
			contextItemWithSource("accepted", "e1", "retrieval", 0.42, "The ZXQ capsule is in the blue box."),
		},
		AssessedItems: []domain.ContextItem{
			contextItemWithSource("accepted", "e1", "retrieval", 0.42, "The ZXQ capsule is in the blue box."),
		},
		MergedItems: []domain.ContextItem{
			contextItemWithSource("accepted", "e1", "retrieval", 0.42, "The ZXQ capsule is in the blue box."),
			contextItemWithSource("unassessed", "e2", "retrieval", 0.99, "This candidate did not pass assessment."),
		},
	}
	state.EnsureTrace()

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if containsHitFact(state.Hits, "unassessed") {
		t.Fatalf("reranker-injected unassessed hit must be filtered, got %+v", hitFactIDs(state.Hits))
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "accepted" {
		t.Fatalf("accepted ranked hit should remain final, got %+v", state.Hits)
	}
	got := detail.(diagnostic.ContextPackDetail)
	if got.RerankedHits != nil {
		for _, snap := range *got.RerankedHits {
			if snap.FactID == "unassessed" {
				t.Fatalf("rerank snapshots must not surface filtered stranger hit: %+v", *got.RerankedHits)
			}
		}
	}
}

func TestContextPackFiltersRerankerInjectedAssessedButNotInputHit(t *testing.T) {
	injected := domain.Hit{
		Ref:      domain.CandidateRef{Kind: domain.GraphNodeAssertion, ID: "assessed-not-input"},
		Fact:     domain.TemporalFact{ID: "assessed-not-input", Kind: domain.KindState, Content: "This assessed item was not in reranker input."},
		Evidence: []domain.EvidenceRef{{ID: "e2", Text: "assessed non-input evidence"}},
		Score:    0.99,
	}
	stage := NewContextPack(injectingReranker{injected: injected})
	state := &read.ReadState{
		Plan:              &domain.QueryPlan{TotalCap: 1},
		Query:             domain.Query{Text: "ZXQ capsule"},
		AssessmentApplied: true,
		Ranked: []domain.ContextItem{
			contextItemWithSource("accepted", "e1", "retrieval", 0.42, "The ZXQ capsule is in the blue box."),
		},
		AssessedItems: []domain.ContextItem{
			contextItemWithSource("accepted", "e1", "retrieval", 0.42, "The ZXQ capsule is in the blue box."),
			contextItemWithSource("assessed-not-input", "e2", "retrieval", 0.99, "This assessed item was not ranked for reranker input."),
		},
	}
	state.EnsureTrace()

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if containsHitFact(state.Hits, "assessed-not-input") {
		t.Fatalf("reranker-injected assessed non-input hit must be filtered, got %+v", hitFactIDs(state.Hits))
	}
	got := detail.(diagnostic.ContextPackDetail)
	if got.RerankedHits != nil {
		for _, snap := range *got.RerankedHits {
			if snap.FactID == "assessed-not-input" {
				t.Fatalf("rerank snapshots must not surface assessed non-input hit: %+v", *got.RerankedHits)
			}
		}
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
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "distractor" {
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

func TestContextPackKeepsBoundedSameEvidenceSiblings(t *testing.T) {
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
		t.Fatalf("want two final hits after bounded same-evidence grouping, got %+v", state.Hits)
	}
	if state.Hits[0].Fact.ID != "a" || state.Hits[1].Fact.ID != "b" {
		t.Fatalf("assessed same-evidence siblings should be bounded but not semantically deduped, got %+v", state.Hits)
	}
}

func TestContextPackKeepsComplementarySiblingFactsFromSameEvidence(t *testing.T) {
	stage := NewContextPack(nil)
	shared := domain.EvidenceRef{ID: "e1", Text: "I have a hand-painted bowl. The pattern reminds me of art and self-expression."}
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "What is Avery's hand-painted bowl a reminder of?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "object", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "object", Kind: domain.KindState, Content: "Avery has a hand-painted bowl that has sentimental value.", Subject: "Avery", Entities: []string{"avery", "bowl"}, EvidenceRefs: []domain.EvidenceRef{shared}},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "I have a hand-painted bowl."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "meaning", Source: "retrieval", Score: 0.8, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "meaning", Kind: domain.KindNote, Content: "The pattern of Avery's hand-painted bowl reminds her of art and self-expression.", Subject: "Avery", Entities: []string{"avery", "bowl"}, EvidenceRefs: []domain.EvidenceRef{shared}},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "The pattern reminds me of art and self-expression."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "distractor", Source: "retrieval", Score: 0.7, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "distractor", Kind: domain.KindState, Content: "Jordan likes pottery."},
				Evidence:  []domain.EvidenceRef{{ID: "e2", Text: "Jordan likes pottery."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 2 || state.Hits[0].Fact.ID != "object" || state.Hits[1].Fact.ID != "meaning" {
		t.Fatalf("complementary sibling facts from one source turn should survive pack, got %+v", state.Hits)
	}
}

func TestContextPackDoesNotRescueSameMessageCluster(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 3},
		Query: domain.Query{Text: "What did the speaker make with clay?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "pots", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"turn-1:span-a"}},
				Fact: domain.TemporalFact{
					ID:       "pots",
					Kind:     domain.KindEvent,
					Content:  "The speaker and family made their own pots at a workshop.",
					Subject:  "the speaker and family",
					Entities: []string{"speaker", "family", "pottery"},
				},
				Evidence: []domain.EvidenceRef{{ID: "turn-1:span-a", MessageID: "turn-1", Text: "We all made our own pots."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "distractor", Source: "retrieval", Score: 0.89, EvidenceIDs: []string{"turn-9:span-a"}},
				Fact: domain.TemporalFact{
					ID:       "distractor",
					Kind:     domain.KindState,
					Content:  "The speaker's family is resilient.",
					Subject:  "the speaker's family",
					Entities: []string{"speaker", "family"},
				},
				Evidence: []domain.EvidenceRef{{ID: "turn-9:span-a", MessageID: "turn-9", Text: "They're resilient."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "other", Source: "retrieval", Score: 0.88, EvidenceIDs: []string{"turn-10:span-a"}},
				Fact: domain.TemporalFact{
					ID:      "other",
					Kind:    domain.KindEvent,
					Content: "The speaker attended another event.",
					Subject: "the speaker",
				},
				Evidence: []domain.EvidenceRef{{ID: "turn-10:span-a", MessageID: "turn-10", Text: "The other event was busy."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "clay", Source: "retrieval", Score: 0.2, EvidenceIDs: []string{"turn-1:span-b"}},
				Fact: domain.TemporalFact{
					ID:       "clay",
					Kind:     domain.KindEvent,
					Content:  "The speaker's family was excited to make something with clay.",
					Subject:  "the speaker's family",
					Entities: []string{"speaker", "family", "clay"},
				},
				Evidence: []domain.EvidenceRef{{ID: "turn-1:span-b", MessageID: "turn-1", Text: "They were so excited to make something with clay."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) < 2 {
		t.Fatalf("expected packed hits, got %+v", state.Hits)
	}
	if containsHitFact(state.Hits, "clay") {
		t.Fatalf("same-message cluster should not rescue lower-ranked sibling, got order %v", hitFactIDs(state.Hits))
	}
}

func TestContextPackEvidenceGroupRequiresStructuredSourceID(t *testing.T) {
	if group := primaryEvidenceGroup(domain.Hit{
		Evidence: []domain.EvidenceRef{{ID: "turn-1:span-a", Text: "same source-shaped id"}},
	}); group != "" {
		t.Fatalf("evidence group should not be inferred from evidence ID shape, got %q", group)
	}
	if group := primaryEvidenceGroup(domain.Hit{
		Evidence: []domain.EvidenceRef{{ID: "turn-1:span-a", MessageID: "turn-1", Text: "explicit source"}},
	}); group != "msg:turn-1" {
		t.Fatalf("evidence group should use explicit message id, got %q", group)
	}
	if group := primaryEvidenceGroup(domain.Hit{
		Evidence: []domain.EvidenceRef{{ID: "span-a", ObservationID: "obs-1", MessageID: "turn-1", Text: "explicit observation"}},
	}); group != "obs:obs-1" {
		t.Fatalf("observation id should take precedence, got %q", group)
	}
}

func TestContextPackDoesNotUseSameMessageClusterAsRelevance(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "What object did the speaker store?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "anchor", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"turn-1:span-a"}},
				Fact:      domain.TemporalFact{ID: "anchor", Kind: domain.KindState, Content: "Avery stored a field compass.", Subject: "Avery", Entities: []string{"avery", "compass"}},
				Evidence:  []domain.EvidenceRef{{ID: "turn-1:span-a", MessageID: "turn-1", Text: "I stored the field compass."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "distractor", Source: "retrieval", Score: 0.89, EvidenceIDs: []string{"turn-9:span-a"}},
				Fact:      domain.TemporalFact{ID: "distractor", Kind: domain.KindState, Content: "Avery owns a notebook.", Subject: "Avery", Entities: []string{"avery", "notebook"}},
				Evidence:  []domain.EvidenceRef{{ID: "turn-9:span-a", MessageID: "turn-9", Text: "I own a notebook."}},
			},
			{
				Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: "same-message-noise", Source: "retrieval", Score: 0.1, EvidenceIDs: []string{"turn-1:span-b"}},
				Fact:      domain.TemporalFact{ID: "same-message-noise", Kind: domain.KindState, Content: "The archive room has stored boxes.", Subject: "archive room", Entities: []string{"archive", "boxes"}},
				Evidence:  []domain.EvidenceRef{{ID: "turn-1:span-b", MessageID: "turn-1", Text: "The archive room has stored boxes."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 2 || state.Hits[1].Fact.ID != "distractor" {
		t.Fatalf("same-message cluster should not rescue lower-scored noise, got order %v", hitFactIDs(state.Hits))
	}
}

func TestContextPackCandidatesKeepAssessedSameEvidenceSiblings(t *testing.T) {
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
	if !got["new"] || !got["old"] {
		t.Fatalf("assessed same-evidence siblings should not be collapsed by query-surface heuristics, got %+v", candidates)
	}
}

func TestContextPackKeepsSourceDiversity(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:              &domain.QueryPlan{TotalCap: 3},
		Query:             domain.Query{Text: "What did Alice say about woodworking class?"},
		AssessmentApplied: true,
		Ranked: []domain.ContextItem{
			contextItemWithSource("retrieval-1", "e1", "retrieval", 0.90, "Alice discussed woodworking class logistics."),
			contextItemWithSource("retrieval-2", "e2", "retrieval", 0.88, "Alice discussed woodworking class timing."),
			contextItemWithSource("retrieval-3", "e3", "retrieval", 0.86, "Alice discussed woodworking class supplies."),
		},
		AssessedItems: []domain.ContextItem{
			contextItemWithSource("retrieval-1", "e1", "retrieval", 0.90, "Alice discussed woodworking class logistics."),
			contextItemWithSource("retrieval-2", "e2", "retrieval", 0.88, "Alice discussed woodworking class timing."),
			contextItemWithSource("retrieval-3", "e3", "retrieval", 0.86, "Alice discussed woodworking class supplies."),
			contextItemWithSource("graph-1", "e4", "graph", 0.87, "Alice discussed woodworking class project details."),
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

func TestContextPackDoesNotFallbackWhenTrustFilteredAllItems(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:           &domain.QueryPlan{TotalCap: 3},
		Query:          domain.Query{Text: "secret"},
		PolicyFiltered: true,
		MergedItems: []domain.ContextItem{
			contextItemWithSource("secret", "e1", "retrieval", 0.9, "secret fact"),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 0 {
		t.Fatalf("context pack must preserve empty policy-filtered set, got %+v", state.Hits)
	}
}

func TestContextPackDoesNotRescueRejectedCandidateFromMergedPool(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:              &domain.QueryPlan{TotalCap: 2},
		Query:             domain.Query{Text: "ZXQ capsule"},
		AssessmentApplied: true,
		Ranked: []domain.ContextItem{
			contextItemWithSource("accepted", "e1", "retrieval", 0.42, "The ZXQ capsule is in the blue box."),
		},
		AssessedItems: []domain.ContextItem{
			contextItemWithSource("accepted", "e1", "retrieval", 0.42, "The ZXQ capsule is in the blue box."),
		},
		MergedItems: []domain.ContextItem{
			contextItemWithSource("accepted", "e1", "retrieval", 0.42, "The ZXQ capsule is in the blue box."),
			contextItemWithSource("rejected", "e2", "retrieval", 0.99, "The materials science course used a blue capsule."),
		},
	}
	state.EnsureTrace()

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if containsHitFact(state.Hits, "rejected") {
		t.Fatalf("context pack must not rescue candidate rejected before rank, got %+v", hitFactIDs(state.Hits))
	}
	got := detail.(diagnostic.ContextPackDetail)
	if got.PackTrace != nil {
		for _, snap := range *got.PackTrace {
			if snap.FactID == "rejected" {
				t.Fatalf("context pack trace must not surface rejected candidate as pack candidate: %+v", *got.PackTrace)
			}
		}
	}
}

func TestContextPackPreservesRankOutputAnchorsBeforeDiversityFill(t *testing.T) {
	ordered := []domain.Hit{
		{
			Fact:     domain.TemporalFact{ID: "gold", Kind: domain.KindEvent, Content: "Alice went to the museum on Tuesday."},
			Score:    0.20,
			Sources:  []string{"retrieval"},
			Evidence: []domain.EvidenceRef{{ID: "e-gold", Text: "I went to the museum on Tuesday."}},
		},
	}
	pool := []domain.Hit{
		{
			Fact:     domain.TemporalFact{ID: "distractor", Kind: domain.KindNote, Content: "Alice received kind encouragement."},
			Score:    0.95,
			Sources:  []string{"entity", "graph", "profile"},
			Evidence: []domain.EvidenceRef{{ID: "e-distractor", Text: "Great job, Alice!"}},
		},
		ordered[0],
	}

	got := packRecallContextWithFeaturesAndDetail(ordered, pool, 1)
	if len(got) != 1 || got[0].Fact.ID != "gold" {
		t.Fatalf("rank_output anchor should survive higher-scored projection distractor, got %+v", got)
	}
}

func TestContextPackDoesNotRouteFilterRankOutput(t *testing.T) {
	ordered := []domain.Hit{
		contextHitWithSources("entity-only", "e-entity", 0.40, []string{"entity"}),
	}
	pool := []domain.Hit{
		contextHitWithSources("retrieved", "e-retrieved", 0.95, []string{"retrieval"}),
		ordered[0],
	}

	got, trace := packRecallContextWithIntentTrace(domain.QueryIntent{}, ordered, pool, 1)
	if len(got) != 1 || got[0].Fact.ID != "entity-only" {
		t.Fatalf("rank_output should not be route-filtered by context pack, got %+v", got)
	}
	var entityTrace diagnostic.CandidateSnapshot
	for _, snap := range trace {
		if snap.FactID == "entity-only" {
			entityTrace = snap
			break
		}
	}
	if entityTrace.RankOutputRank != 1 || entityTrace.ContextPackRank != 1 {
		t.Fatalf("entity-only trace should show rank_output candidate packed, got %+v", entityTrace)
	}
}

func TestContextPackTraceRecordsRanksRoutesAndDrops(t *testing.T) {
	ordered := []domain.Hit{
		contextHitWithSources("gold", "e-gold", 0.90, []string{"retrieval"}),
		contextHitWithSources("filler", "e-filler", 0.80, []string{"retrieval"}),
	}
	pool := []domain.Hit{
		contextHitWithSources("projection", "e-projection", 0.95, []string{"entity", "graph"}),
	}

	got, trace := packRecallContextWithTrace(ordered, pool, 1)
	if len(got) != 1 || got[0].Fact.ID != "gold" {
		t.Fatalf("packed hits = %+v", got)
	}
	if len(trace) != 3 {
		t.Fatalf("trace len = %d, want 3: %+v", len(trace), trace)
	}
	var gold, projection diagnostic.CandidateSnapshot
	for _, snap := range trace {
		switch snap.FactID {
		case "gold":
			gold = snap
		case "projection":
			projection = snap
		}
	}
	if gold.RankOutputRank != 1 || gold.ContextPackRank != 1 || gold.PrimarySource != "retrieval" {
		t.Fatalf("gold trace = %+v", gold)
	}
	if projection.DroppedReason == "" || projection.ContextPackRank != 0 || len(projection.ProjectionRoutes) != 2 {
		t.Fatalf("projection trace should record drop and routes, got %+v", projection)
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

func TestContextPackKeepsRawObservationBehindStructuredEvidence(t *testing.T) {
	hits := []domain.Hit{
		{
			Fact:     domain.TemporalFact{ID: "structured", Kind: domain.KindEvent, Content: "Avery went to the workshop on Tuesday."},
			Score:    0.02,
			Sources:  []string{"retrieval"},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "I went to the workshop on Tuesday."}},
		},
		{
			Ref: domain.CandidateRef{Kind: domain.GraphNodeObservation, ID: "obs"},
			Observation: domain.Observation{
				ID:   "obs",
				Text: "I went to a similar workshop last Friday and it was useful.",
			},
			Score:    1.0,
			Sources:  []string{"observation"},
			Evidence: []domain.EvidenceRef{{ID: "obs", ObservationID: "obs", Text: "I went to a similar workshop last Friday and it was useful."}},
		},
	}

	got := packRecallContextWithFeaturesAndDetail(hits, hits, 2)
	if len(got) != 2 {
		t.Fatalf("packed hits = %+v", got)
	}
	if got[0].Fact.ID != "structured" || got[1].Ref.Kind != domain.GraphNodeObservation {
		t.Fatalf("raw observation should support structured evidence, not outrank it: %+v", got)
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

func TestBuildGroundedHitsEvidencePacketIsCapped(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	observations := observationstore.New()
	links := linkstore.New()
	hit := domain.Hit{
		Ref:  domain.CandidateRef{Kind: domain.GraphNodeAssertion, ID: "fact", Scope: scope},
		Fact: domain.TemporalFact{ID: "fact", Scope: scope, Kind: domain.KindState, Content: "Alice bought woodworking."},
	}
	for i := 0; i < maxEvidencePacketObservations+4; i++ {
		obsID := "obs-" + strconv.Itoa(i)
		hit.Evidence = append(hit.Evidence, domain.EvidenceRef{
			ID:            "e-" + strconv.Itoa(i),
			ObservationID: obsID,
			Text:          "evidence " + strconv.Itoa(i),
		})
		if err := observations.Append(ctx, []domain.Observation{{
			ID:    obsID,
			Scope: scope,
			Text:  "observation " + strconv.Itoa(i),
		}}); err != nil {
			t.Fatalf("observations.Append: %v", err)
		}
	}
	var factLinks []domain.FactLink
	for i := 0; i < maxEvidencePacketLinks+4; i++ {
		factLinks = append(factLinks, domain.FactLink{
			ID:    "link-" + strconv.Itoa(i),
			Scope: scope,
			Type:  domain.LinkSupports,
			From:  domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "fact"},
			To:    domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: "obs-" + strconv.Itoa(i)},
		})
	}
	if err := links.Append(ctx, factLinks); err != nil {
		t.Fatalf("links.Append: %v", err)
	}

	packet := NewBuildGroundedHits(WithGroundedHitGraph(observations, links)).buildEvidencePacket(ctx, scope, hit)
	if len(packet.EvidenceRefs) != maxHitEvidenceRefs {
		t.Fatalf("packet evidence refs = %d, want %d", len(packet.EvidenceRefs), maxHitEvidenceRefs)
	}
	if len(packet.Links) != maxEvidencePacketLinks {
		t.Fatalf("packet links = %d, want %d", len(packet.Links), maxEvidencePacketLinks)
	}
	if len(packet.Observations) != maxEvidencePacketObservations {
		t.Fatalf("packet observations = %d, want %d", len(packet.Observations), maxEvidencePacketObservations)
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

func hitFactIDs(hits []domain.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Fact.ID)
	}
	return out
}

func containsHitFact(hits []domain.Hit, id string) bool {
	for _, hit := range hits {
		if hit.Fact.ID == id {
			return true
		}
	}
	return false
}

func sameStringSlice(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func contextItemWithSource(id, evidenceID, source string, score float64, text string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: id, Source: source, Score: score, EvidenceIDs: []string{evidenceID}},
		Fact:      domain.TemporalFact{ID: id, Kind: domain.KindState, Content: text},
		Evidence:  []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}

func contextHitWithSources(id, evidenceID string, score float64, sources []string) domain.Hit {
	text := id + " evidence"
	return domain.Hit{
		Fact:     domain.TemporalFact{ID: id, Kind: domain.KindState, Content: text},
		Score:    score,
		Sources:  append([]string(nil), sources...),
		Evidence: []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}
