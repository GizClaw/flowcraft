package stages

import (
	"context"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

// FederationMerge deduplicates materialized items across sub-scopes
// (Phase D.5). Single-scope reads Skip via Conditional.
type FederationMerge struct{}

// NewFederationMerge constructs a FederationMerge stage.
func NewFederationMerge() *FederationMerge { return &FederationMerge{} }

// Name implements pipeline.Stage.
func (FederationMerge) Name() string { return "federation_merge" }

// Skip implements pipeline.Conditional.
func (FederationMerge) Skip(_ context.Context, state *read.ReadState) (bool, diagnostic.StageDetail) {
	if state == nil || len(state.SubScopeStates) <= 1 {
		read.PromoteMergedItems(state)
		return true, diagnostic.FederationMergeDetail{}
	}
	return false, nil
}

// Run implements pipeline.Stage.
func (FederationMerge) Run(_ context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	var pool []domain.ContextItem
	inputCount := 0
	for i := range state.SubScopeStates {
		inputCount += len(state.SubScopeStates[i].Materialized)
		pool = append(pool, state.SubScopeStates[i].Materialized...)
	}
	merged, dropped := mergeFederationItems(pool, topKForMerge(state))
	state.MergedItems = merged
	detail := diagnostic.FederationMergeDetail{
		InputCount:     inputCount,
		AfterDedup:     len(merged),
		AfterTopK:      len(merged),
		DroppedByDedup: dropped,
		Latency:        time.Since(started),
	}
	if snapshotsEnabled(state) {
		detail.Items = candidateSnapshotPtr(contextItemSnapshots(merged))
	}
	return detail, nil
}

func topKForMerge(state *read.ReadState) int {
	if state.Plan != nil && state.Plan.TotalCap > 0 {
		return state.Plan.TotalCap
	}
	if state.Intent != nil && state.Intent.Limit > 0 {
		return state.Intent.Limit
	}
	return 0
}

// mergeFederationItems dedupes by FactID keeping max score, sorts score
// desc with FactID tie-break, then truncates to topK (v1 contract).
func mergeFederationItems(items []domain.ContextItem, topK int) ([]domain.ContextItem, int) {
	if len(items) == 0 {
		return nil, 0
	}
	best := make(map[string]int, len(items))
	for i, item := range items {
		id := item.Fact.ID
		if id == "" {
			id = item.Candidate.FactID
		}
		if id == "" {
			continue
		}
		prev, ok := best[id]
		if !ok || item.Candidate.Score > items[prev].Candidate.Score {
			best[id] = i
		}
	}
	out := make([]domain.ContextItem, 0, len(best))
	for _, idx := range best {
		out = append(out, items[idx])
	}
	dropped := len(items) - len(out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Candidate.Score != out[j].Candidate.Score {
			return out[i].Candidate.Score > out[j].Candidate.Score
		}
		return out[i].Fact.ID < out[j].Fact.ID
	})
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out, dropped
}

var (
	_ pipeline.Stage[*read.ReadState]       = (*FederationMerge)(nil)
	_ pipeline.Conditional[*read.ReadState] = (*FederationMerge)(nil)
)
