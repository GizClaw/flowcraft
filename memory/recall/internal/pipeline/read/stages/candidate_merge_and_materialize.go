package stages

import (
	"context"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// FusionCapFunc computes the per-source fusion pool cap from the plan's final
// hit cap.
type FusionCapFunc func(finalCap int) int

// CandidateMergeAndMaterialize builds the candidate pool used by policy,
// ranking, and context packing: source results are fused per scope,
// materialized from the canonical store, then deduped across scopes.
type CandidateMergeAndMaterialize struct {
	fuser        port.Fuser
	fusionOpts   port.FusionOptions
	capFunc      FusionCapFunc
	materializer port.Materializer
}

func NewCandidateMergeAndMaterialize(fuser port.Fuser, fusionOpts port.FusionOptions, capFunc FusionCapFunc, materializer port.Materializer) *CandidateMergeAndMaterialize {
	return &CandidateMergeAndMaterialize{
		fuser:        fuser,
		fusionOpts:   fusionOpts,
		capFunc:      capFunc,
		materializer: materializer,
	}
}

func (CandidateMergeAndMaterialize) Name() string { return "candidate_merge_and_materialize" }

func (s *CandidateMergeAndMaterialize) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	detail := diagnostic.CandidateMergeAndMaterializeDetail{}
	if state == nil || state.Plan == nil {
		return detail, nil
	}
	captureSnapshots := snapshotsEnabled(state)
	var pool []domain.ContextItem
	var aggregatedDrops []diagnostic.CandidateDrop
	for i := range state.SubScopeStates {
		subStarted := time.Now()
		sub := &state.SubScopeStates[i]
		inputCount := 0
		for _, res := range sub.SourceResults {
			inputCount += len(res.Candidates)
		}
		detail.InputCount += inputCount
		opts := s.fusionOpts
		if opts.TotalCap == 0 && s.capFunc != nil {
			opts.TotalCap = s.capFunc(state.Plan.TotalCap)
		}
		opts.SourceFloors = mergeSourceFloors(opts.SourceFloors, fusionSourceFloors(state.Plan.Intent.Features))
		fused, drops, err := s.fuser.Fuse(ctx, sub.SourceResults, opts)
		if err != nil {
			detail.SubScopes = append(detail.SubScopes, diagnostic.SubScopeRun{Scope: sub.Scope.CanonicalKey(), Err: err.Error(), Latency: time.Since(subStarted)})
			return detail, err
		}
		sub.Candidates = fused
		sub.CandidateDrops = drops
		detail.CandidateCount += len(fused)
		detail.Drops = append(detail.Drops, drops...)
		aggregatedDrops = append(aggregatedDrops, drops...)
		if captureSnapshots {
			detail.CandidateSnapshots = appendSnapshotPtr(detail.CandidateSnapshots, candidateSnapshots(fused))
		}

		items, matDrops, err := s.materializer.Materialize(ctx, fused)
		if err != nil {
			detail.SubScopes = append(detail.SubScopes, diagnostic.SubScopeRun{Scope: sub.Scope.CanonicalKey(), Err: err.Error(), Latency: time.Since(subStarted)})
			return detail, err
		}
		if !state.Query.IncludeRetired {
			items, matDrops = filterRetiredItems(items, matDrops, state.Now)
		}
		sub.Materialized = items
		sub.MaterializeDrops = matDrops
		detail.MaterializedCount += len(items)
		detail.Drops = append(detail.Drops, matDrops...)
		aggregatedDrops = append(aggregatedDrops, matDrops...)
		pool = append(pool, items...)
		if captureSnapshots {
			detail.MaterializedSnapshots = appendSnapshotPtr(detail.MaterializedSnapshots, contextItemSnapshots(items))
		}
		detail.SubScopes = append(detail.SubScopes, diagnostic.SubScopeRun{
			Scope:         sub.Scope.CanonicalKey(),
			SourceResults: len(sub.SourceResults),
			Materialized:  len(items),
			Latency:       time.Since(subStarted),
		})
	}
	state.MaterializeDrops = aggregatedDrops
	merged, dropped := mergeFederationItems(pool, topKForMerge(state))
	state.MergedItems = merged
	detail.OutputCount = len(merged)
	detail.DroppedByDedup = dropped
	detail.Latency = time.Since(started)
	if captureSnapshots {
		detail.Output = candidateSnapshotPtr(contextItemSnapshots(merged))
	}
	return detail, nil
}

func mergeSourceFloors(base, extra map[string]int) map[string]int {
	out := make(map[string]int, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		if out[k] < v {
			out[k] = v
		}
	}
	return out
}

func fusionSourceFloors(features domain.QueryFeatures) map[string]int {
	floors := map[string]int{}
	if features.HasTimeSignal() || features.NumericIntent {
		floors[planner.SourceRetrieval] = 5
	}
	if planner.DirectTimelineDateIntent(features) {
		floors[planner.SourceTimeline] = 3
	}
	if len(floors) == 0 {
		return nil
	}
	return floors
}

func filterRetiredItems(items []domain.ContextItem, drops []diagnostic.CandidateDrop, now time.Time) ([]domain.ContextItem, []diagnostic.CandidateDrop) {
	if len(items) == 0 {
		return items, drops
	}
	kept := make([]domain.ContextItem, 0, len(items))
	for _, item := range items {
		if domain.IsRetired(item.Fact, now) {
			drops = append(drops, diagnostic.CandidateDrop{
				Stage:  "candidate_materialize",
				Reason: diagnostic.DropRetired,
				FactID: item.Fact.ID,
				Source: item.Candidate.Source,
			})
			continue
		}
		kept = append(kept, item)
	}
	return kept, drops
}

func topKForMerge(state *read.ReadState) int {
	if state.Plan != nil && state.Plan.TotalCap > 0 {
		return deterministicRankCap(state.Plan.TotalCap, false)
	}
	if state.Intent != nil && state.Intent.Limit > 0 {
		return deterministicRankCap(state.Intent.Limit, false)
	}
	return 0
}

func mergeFederationItems(items []domain.ContextItem, topK int) ([]domain.ContextItem, int) {
	if len(items) == 0 {
		return nil, 0
	}
	best := make(map[string]int, len(items))
	for i, item := range items {
		id := item.Fact.ID
		if id == "" {
			id = item.Candidate.ID
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

var _ pipeline.Stage[*read.ReadState] = (*CandidateMergeAndMaterialize)(nil)
