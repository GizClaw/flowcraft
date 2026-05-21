package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// FederationFanout expands EffectiveFederation() and runs source_fanout,
// fuse, and materialize per sub-scope (Phase D.5).
//
// Cluster G, D2 (2026-05-21): plan is GLOBAL. This stage no longer
// re-invokes planner.Plan per sub-scope; it consumes state.Plan
// (populated by the upstream Plan stage with cross-scope entity
// hints already merged). Federation is a data dimension — strategy
// stays unified across sub-scopes so rank / fuse / build_hits and
// source-fanout share the same plan pointer and avoid the split-brain
// where the global plan and per-scope plans disagreed.
type FederationFanout struct {
	sources      SourceProvider
	fuser        port.Fuser
	fusionOpts   port.FusionOptions
	capFunc      FusionCapFunc
	materializer port.Materializer
}

// NewFederationFanout constructs a FederationFanout stage.
func NewFederationFanout(
	sources SourceProvider,
	fuser port.Fuser,
	fusionOpts port.FusionOptions,
	capFunc FusionCapFunc,
	materializer port.Materializer,
) *FederationFanout {
	return &FederationFanout{
		sources:      sources,
		fuser:        fuser,
		fusionOpts:   fusionOpts,
		capFunc:      capFunc,
		materializer: materializer,
	}
}

// Name implements pipeline.Stage.
func (FederationFanout) Name() string { return "federation_fanout" }

// Run implements pipeline.Stage.
func (s *FederationFanout) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	if state.Intent == nil {
		return diagnostic.FederationFanoutDetail{}, nil
	}
	scopes := state.Scope.EffectiveFederation()
	fastPath := len(scopes) <= 1
	detail := diagnostic.FederationFanoutDetail{FastPath: fastPath}

	if state.Plan == nil {
		// Plan stage produced no plan (e.g. nil Intent path already
		// returned above; defensive guard). Emit an empty detail.
		return detail, nil
	}
	plan := *state.Plan

	srcs := s.sources()
	byName := make(map[string]port.Source, len(srcs))
	for _, src := range srcs {
		byName[src.Name()] = src
	}

	state.SubScopeStates = make([]read.SubScopeState, 0, len(scopes))
	var sourceErrs []error

	for _, sc := range scopes {
		started := time.Now()
		sub := read.SubScopeState{Scope: sc, FastPath: fastPath}
		// Cluster G, D2 (2026-05-21): plan strategy
		// (SourceOrder / SourceBudgets / LensWeights / TotalCap)
		// is global, but each source needs the sub-scope's Scope
		// to build its scope-isolation filter / namespace. We
		// shallow-copy the global plan and only override
		// Intent.Scope so sources see the correct per-sub-scope
		// partition while the planner-chosen strategy stays
		// uniform.
		subPlan := plan
		subPlan.Intent.Scope = sc
		sub.Plan = &subPlan

		results := make([]domain.SourceResult, 0, len(subPlan.SourceOrder))
		for _, name := range subPlan.SourceOrder {
			src, ok := byName[name]
			if !ok {
				continue
			}
			res := src.Query(ctx, subPlan)
			results = append(results, res)
			if res.Err != nil {
				sourceErrs = append(sourceErrs, fmt.Errorf("%s: %w", res.Source, res.Err))
			}
		}
		sub.SourceResults = results
		for _, res := range results {
			errStr := ""
			if res.Err != nil {
				errStr = res.Err.Error()
			}
			detail.Sources = append(detail.Sources, diagnostic.SourceResult{
				Lens:       res.Source,
				Candidates: len(res.Candidates),
				Latency:    res.Latency,
				Err:        errStr,
			})
		}

		opts := s.fusionOpts
		if opts.TotalCap == 0 && s.capFunc != nil {
			opts.TotalCap = s.capFunc(subPlan.TotalCap)
		}
		fused, drops, err := s.fuser.Fuse(ctx, results, opts)
		if err != nil {
			run := diagnostic.SubScopeRun{Scope: sc.CanonicalKey(), Err: err.Error(), Latency: time.Since(started)}
			detail.SubScopes = append(detail.SubScopes, run)
			return detail, err
		}
		sub.Fused = fused
		sub.FusionDrops = drops
		detail.FusedCandidates += len(fused)
		detail.Drops = append(detail.Drops, drops...)

		items, matDrops, err := s.materializer.Materialize(ctx, fused)
		if err != nil {
			run := diagnostic.SubScopeRun{Scope: sc.CanonicalKey(), Err: err.Error(), Latency: time.Since(started)}
			detail.SubScopes = append(detail.SubScopes, run)
			return detail, err
		}
		if !state.Query.IncludeRetired {
			items, matDrops = filterRetiredItems(items, matDrops, state.Now)
		}
		sub.Materialized = items
		sub.MaterializeDrops = matDrops
		detail.Materialized += len(items)
		detail.Drops = append(detail.Drops, matDrops...)

		detail.SubScopes = append(detail.SubScopes, diagnostic.SubScopeRun{
			Scope:         sc.CanonicalKey(),
			SourceResults: len(results),
			Materialized:  len(items),
			Latency:       time.Since(started),
		})
		state.SubScopeStates = append(state.SubScopeStates, sub)
	}

	totalRows := 0
	totalCandidates := 0
	for _, sub := range state.SubScopeStates {
		totalRows += len(sub.SourceResults)
		for _, res := range sub.SourceResults {
			totalCandidates += len(res.Candidates)
		}
	}
	if len(sourceErrs) > 0 && len(sourceErrs) == totalRows && totalCandidates == 0 {
		return detail, read.AllSourcesFailed(sourceErrs)
	}
	return detail, nil
}

func filterRetiredItems(items []domain.ContextItem, drops []diagnostic.CandidateDrop, now time.Time) ([]domain.ContextItem, []diagnostic.CandidateDrop) {
	if len(items) == 0 {
		return items, drops
	}
	kept := make([]domain.ContextItem, 0, len(items))
	for _, item := range items {
		if domain.IsRetired(item.Fact, now) {
			drops = append(drops, diagnostic.CandidateDrop{
				Stage:  "materialize",
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

var _ pipeline.Stage[*read.ReadState] = (*FederationFanout)(nil)
