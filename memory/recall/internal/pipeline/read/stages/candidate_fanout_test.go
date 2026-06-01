package stages

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

type variantRecordingSource struct {
	name  string
	mu    sync.Mutex
	texts []string
}

func (s *variantRecordingSource) Name() string { return s.name }

func (s *variantRecordingSource) Query(_ context.Context, plan domain.QueryPlan) domain.SourceResult {
	s.mu.Lock()
	s.texts = append(s.texts, plan.Intent.Text)
	s.mu.Unlock()
	switch plan.Intent.Text {
	case "What pets does Jordan have?":
		return domain.SourceResult{
			Source: s.name,
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "bailey", Source: s.name, Rank: 1, Score: 0.90, EvidenceIDs: []string{"e1"}},
				{Kind: domain.GraphNodeAssertion, ID: "shared", Source: s.name, Rank: 2, Score: 0.30, EvidenceIDs: []string{"e2"}},
			},
		}
	case "pets Jordan":
		return domain.SourceResult{
			Source: s.name,
			Candidates: []domain.Candidate{
				{Kind: domain.GraphNodeAssertion, ID: "oliver", Source: s.name, Rank: 1, Score: 0.60, EvidenceIDs: []string{"e3"}},
				{Kind: domain.GraphNodeAssertion, ID: "shared", Source: s.name, Rank: 2, Score: 0.70, EvidenceIDs: []string{"e4"}},
			},
		}
	default:
		return domain.SourceResult{Source: s.name}
	}
}

func (s *variantRecordingSource) recordedTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

type cancelingSource struct{}

func (cancelingSource) Name() string { return "retrieval" }

func (cancelingSource) Query(context.Context, domain.QueryPlan) domain.SourceResult {
	return domain.SourceResult{
		Source: "retrieval",
		Candidates: []domain.Candidate{
			{Kind: domain.GraphNodeAssertion, ID: "partial", Source: "retrieval", Rank: 1, Score: 0.9},
		},
		Err: context.DeadlineExceeded,
	}
}

type slowFanoutSource struct {
	name  string
	delay time.Duration
}

func (s slowFanoutSource) Name() string { return s.name }

func (s slowFanoutSource) Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return domain.SourceResult{Source: s.name, Err: ctx.Err()}
	}
	return domain.SourceResult{
		Source: s.name,
		Candidates: []domain.Candidate{{
			Kind:   domain.GraphNodeAssertion,
			ID:     s.name + "-" + plan.Intent.Text,
			Source: s.name,
			Rank:   1,
			Score:  1,
		}},
		Latency: s.delay,
	}
}

func TestCandidateFanoutUsesPlanDrivenQueryVariantsWithoutDuplicateBoost(t *testing.T) {
	src := &variantRecordingSource{name: "retrieval"}
	stage := NewCandidateFanout(func() []port.Source { return []port.Source{src} })
	intent := domain.QueryIntent{Text: "What pets does Jordan have?"}
	plan := domain.QueryPlan{
		Intent:        intent,
		SourceOrder:   []string{"retrieval"},
		SourceBudgets: map[string]int{"retrieval": 9},
		TotalCap:      9,
		TaskIntents:   []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
	}
	state := &read.ReadState{
		Scope:  domain.Scope{RuntimeID: "rt", UserID: "u"},
		Intent: &intent,
		Plan:   &plan,
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if texts := src.recordedTexts(); len(texts) < 2 {
		t.Fatalf("expected source to be queried with variants, got texts %+v", texts)
	}
	results := state.SubScopeStates[0].SourceResults
	if len(results) != 1 {
		t.Fatalf("source results = %+v", results)
	}
	seen := map[string]int{}
	for _, candidate := range results[0].Candidates {
		seen[candidate.ID]++
	}
	if seen["shared"] != 1 {
		t.Fatalf("variant merge should dedupe repeated fact once, got candidates %+v", results[0].Candidates)
	}
	if len(results[0].Candidates) != 3 {
		t.Fatalf("variant merge should widen candidate pool without duplicates, got %+v", results[0].Candidates)
	}
	gotDetail := detail.(diagnostic.CandidateFanoutDetail)
	if len(gotDetail.Sources) != 1 || gotDetail.Sources[0].QueryVariants < 2 {
		t.Fatalf("source diagnostics should expose query variants, got %+v", gotDetail.Sources)
	}
}

func TestCandidateFanoutRunsRetrievalConcurrentlyAndPreservesOrder(t *testing.T) {
	stage := NewCandidateFanout(func() []port.Source {
		return []port.Source{
			slowFanoutSource{name: "retrieval", delay: 80 * time.Millisecond},
			slowFanoutSource{name: "entity", delay: 40 * time.Millisecond},
		}
	})
	intent := domain.QueryIntent{Text: "query"}
	plan := domain.QueryPlan{
		Intent:        intent,
		SourceOrder:   []string{"retrieval", "entity"},
		SourceBudgets: map[string]int{"retrieval": 10, "entity": 10},
		TotalCap:      10,
	}
	state := &read.ReadState{
		Scope:  domain.Scope{RuntimeID: "rt", UserID: "u"},
		Intent: &intent,
		Plan:   &plan,
	}

	started := time.Now()
	detail, err := stage.Run(context.Background(), state)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if elapsed >= 110*time.Millisecond {
		t.Fatalf("retrieval should overlap with structured sources, elapsed %s", elapsed)
	}
	results := state.SubScopeStates[0].SourceResults
	if len(results) != 2 || results[0].Source != "retrieval" || results[1].Source != "entity" {
		t.Fatalf("source results should preserve SourceOrder, got %+v", results)
	}
	gotDetail := detail.(diagnostic.CandidateFanoutDetail)
	if len(gotDetail.Sources) != 2 || gotDetail.Sources[0].Lens != "retrieval" || gotDetail.Sources[1].Lens != "entity" {
		t.Fatalf("source diagnostics should preserve SourceOrder, got %+v", gotDetail.Sources)
	}
}

func TestCandidateFanoutKeepsStructuredSourcesSerial(t *testing.T) {
	stage := NewCandidateFanout(func() []port.Source {
		return []port.Source{
			slowFanoutSource{name: "entity", delay: 40 * time.Millisecond},
			slowFanoutSource{name: "graph", delay: 40 * time.Millisecond},
		}
	})
	intent := domain.QueryIntent{Text: "query"}
	plan := domain.QueryPlan{
		Intent:        intent,
		SourceOrder:   []string{"entity", "graph"},
		SourceBudgets: map[string]int{"entity": 10, "graph": 10},
		TotalCap:      10,
	}
	state := &read.ReadState{
		Scope:  domain.Scope{RuntimeID: "rt", UserID: "u"},
		Intent: &intent,
		Plan:   &plan,
	}

	started := time.Now()
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 75*time.Millisecond {
		t.Fatalf("structured sources should remain serial, elapsed %s", elapsed)
	}
}

func TestQuerySourceWithPlanVariantsRunsVariantsConcurrently(t *testing.T) {
	src := slowFanoutSource{name: "retrieval", delay: 80 * time.Millisecond}
	plan := domain.QueryPlan{
		Intent:        domain.QueryIntent{Text: "What pets does Jordan have?"},
		SourceBudgets: map[string]int{"retrieval": 9},
		TotalCap:      9,
		TaskIntents:   []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
	}

	started := time.Now()
	res := querySourceWithPlanVariants(context.Background(), src, plan)
	elapsed := time.Since(started)
	if elapsed >= 160*time.Millisecond {
		t.Fatalf("query variants should run concurrently, elapsed %s", elapsed)
	}
	if len(res.Candidates) < 2 {
		t.Fatalf("merged variant candidates = %+v", res.Candidates)
	}
}

func TestQuerySourceWithPlanVariantsKeepsStructuredVariantsSerial(t *testing.T) {
	src := slowFanoutSource{name: "entity", delay: 40 * time.Millisecond}
	plan := domain.QueryPlan{
		Intent:        domain.QueryIntent{Text: "What pets does Jordan have?"},
		SourceBudgets: map[string]int{"entity": 9},
		TotalCap:      9,
		TaskIntents:   []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
	}

	started := time.Now()
	res := querySourceWithPlanVariants(context.Background(), src, plan)
	elapsed := time.Since(started)
	if elapsed < 75*time.Millisecond {
		t.Fatalf("structured query variants should remain serial, elapsed %s", elapsed)
	}
	if len(res.Candidates) < 2 {
		t.Fatalf("merged variant candidates = %+v", res.Candidates)
	}
}

func TestCandidateFanoutPropagatesContextErrorEvenWithPartialCandidates(t *testing.T) {
	stage := NewCandidateFanout(func() []port.Source { return []port.Source{cancelingSource{}} })
	intent := domain.QueryIntent{Text: "query"}
	plan := domain.QueryPlan{
		Intent:        intent,
		SourceOrder:   []string{"retrieval"},
		SourceBudgets: map[string]int{"retrieval": 10},
		TotalCap:      10,
	}
	state := &read.ReadState{
		Scope:  domain.Scope{RuntimeID: "rt", UserID: "u"},
		Intent: &intent,
		Plan:   &plan,
	}

	_, err := stage.Run(context.Background(), state)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context deadline should propagate even when source returned partial candidates, got %v", err)
	}
}
