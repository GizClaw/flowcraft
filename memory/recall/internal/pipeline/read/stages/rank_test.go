package stages

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

type recordingRanker struct {
	finalCap int
}

func (r *recordingRanker) Rank(_ context.Context, in port.RankInput) port.RankOutput {
	r.finalCap = in.FinalCap
	items := append([]domain.ContextItem(nil), in.Items...)
	if in.FinalCap > 0 && len(items) > in.FinalCap {
		items = items[:in.FinalCap]
	}
	return port.RankOutput{Items: items}
}

func TestRankExpandsDeterministicPoolWithoutReranker(t *testing.T) {
	rnk := &recordingRanker{}
	stage := NewRank(rnk, false)
	state := &read.ReadState{
		Plan:       &domain.QueryPlan{TotalCap: 10},
		AfterTrust: makeContextItems(50),
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rnk.finalCap != 30 {
		t.Fatalf("rank final cap = %d, want 30", rnk.finalCap)
	}
	if len(state.Ranked) != 30 {
		t.Fatalf("ranked len = %d, want 30", len(state.Ranked))
	}
}

func TestRankLeavesPoolUncappedForReranker(t *testing.T) {
	rnk := &recordingRanker{}
	stage := NewRank(rnk, true)
	state := &read.ReadState{
		Plan:       &domain.QueryPlan{TotalCap: 10},
		AfterTrust: makeContextItems(50),
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rnk.finalCap != 0 {
		t.Fatalf("rank final cap = %d, want 0", rnk.finalCap)
	}
	if len(state.Ranked) != 50 {
		t.Fatalf("ranked len = %d, want 50", len(state.Ranked))
	}
}

func makeContextItems(n int) []domain.ContextItem {
	items := make([]domain.ContextItem, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("fact-%02d", i)
		items = append(items, domain.ContextItem{
			Candidate: domain.Candidate{Kind: domain.GraphNodeAssertion, ID: id},
			Fact:      domain.TemporalFact{ID: id},
		})
	}
	return items
}
