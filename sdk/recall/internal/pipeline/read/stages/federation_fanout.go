package stages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// EntitySnapshotFunc lifts canonical entity hints per sub-scope plan.
type EntitySnapshotFunc func(scope domain.Scope) []port.EntitySnapshot

// FederationFanout expands EffectiveFederation() and runs source_fanout,
// fuse, and materialize per sub-scope (Phase D.5).
type FederationFanout struct {
	sources      SourceProvider
	planner      port.Planner
	graphEnabled bool
	fuser        port.Fuser
	fusionOpts   port.FusionOptions
	capFunc      FusionCapFunc
	materializer port.Materializer
	entitySnap   EntitySnapshotFunc
}

// NewFederationFanout constructs a FederationFanout stage.
func NewFederationFanout(
	sources SourceProvider,
	planner port.Planner,
	graphEnabled bool,
	fuser port.Fuser,
	fusionOpts port.FusionOptions,
	capFunc FusionCapFunc,
	materializer port.Materializer,
	entitySnap EntitySnapshotFunc,
) *FederationFanout {
	return &FederationFanout{
		sources:      sources,
		planner:      planner,
		graphEnabled: graphEnabled,
		fuser:        fuser,
		fusionOpts:   fusionOpts,
		capFunc:      capFunc,
		materializer: materializer,
		entitySnap:   entitySnap,
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
		plan, err := s.planner.Plan(ctx, port.PlannerInput{
			Scope:        sc,
			Text:         state.Intent.Text,
			Entities:     state.Intent.Entities,
			Limit:        state.Intent.Limit,
			Subject:      state.Intent.Subject,
			Predicate:    state.Intent.Predicate,
			Object:       state.Intent.Object,
			Kinds:        state.Intent.Kinds,
			TimeRange:    state.Intent.TimeRange,
			GraphEnabled: s.graphEnabled,
			GraphHops:    state.Query.GraphHops,
		})
		if err != nil {
			run := diagnostic.SubScopeRun{Scope: sc.CanonicalKey(), Err: err.Error(), Latency: time.Since(started)}
			detail.SubScopes = append(detail.SubScopes, run)
			return detail, err
		}
		sub.Plan = &plan

		results := make([]domain.SourceResult, 0, len(plan.SourceOrder))
		for _, name := range plan.SourceOrder {
			src, ok := byName[name]
			if !ok {
				continue
			}
			res := src.Query(ctx, plan)
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
			opts.TotalCap = s.capFunc(plan.TotalCap)
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

		_ = s.entitySnap // reserved for future planner entity hints
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
		return detail, fmt.Errorf("recall.Recall: all sources failed: %w", errors.Join(sourceErrs...))
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
