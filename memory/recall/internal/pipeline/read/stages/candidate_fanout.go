package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// SourceProvider returns the live source list (memory.sources may be replaced
// after New, e.g. in tests).
type SourceProvider func() []port.Source

// CandidateFanout queries every planned source for every effective scope. It
// only produces raw candidates; fusion/materialization live in the next stage.
type CandidateFanout struct {
	sources SourceProvider
}

func NewCandidateFanout(sources SourceProvider) *CandidateFanout {
	return &CandidateFanout{sources: sources}
}

func (CandidateFanout) Name() string { return "candidate_fanout" }

func (s *CandidateFanout) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	if state == nil || state.Intent == nil || state.Plan == nil {
		return diagnostic.CandidateFanoutDetail{}, nil
	}
	srcs := s.sources()
	byName := make(map[string]port.Source, len(srcs))
	for _, src := range srcs {
		byName[src.Name()] = src
	}

	scopes := state.Scope.EffectiveFederation()
	fastPath := len(scopes) <= 1
	state.SubScopeStates = make([]read.SubScopeState, 0, len(scopes))
	detail := diagnostic.CandidateFanoutDetail{}
	var sourceErrs []error
	totalRows := 0
	totalCandidates := 0
	captureSnapshots := snapshotsEnabled(state)

	for _, sc := range scopes {
		started := time.Now()
		subPlan := *state.Plan
		subPlan.Intent.Scope = sc
		sub := read.SubScopeState{Scope: sc, Plan: &subPlan, FastPath: fastPath}
		results := make([]domain.SourceResult, 0, len(subPlan.SourceOrder))
		for _, name := range subPlan.SourceOrder {
			src, ok := byName[name]
			if !ok {
				continue
			}
			res := querySourceWithPlanVariants(ctx, src, subPlan)
			results = append(results, res)
			totalRows++
			totalCandidates += len(res.Candidates)
			if res.Err != nil {
				sourceErrs = append(sourceErrs, fmt.Errorf("%s: %w", res.Source, res.Err))
			}
			row := diagnostic.SourceResult{
				Lens:          res.Source,
				Candidates:    len(res.Candidates),
				QueryVariants: len(sourceFanoutPlanVariants(subPlan, name)),
				Latency:       res.Latency,
			}
			if res.Err != nil {
				row.Err = res.Err.Error()
			}
			if captureSnapshots {
				row.Snapshots = candidateSnapshotPtr(candidateSnapshots(res.Candidates))
			}
			detail.Sources = append(detail.Sources, row)
		}
		sub.SourceResults = results
		state.SubScopeStates = append(state.SubScopeStates, sub)
		detail.SubScopes = append(detail.SubScopes, diagnostic.SubScopeRun{
			Scope:         sc.CanonicalKey(),
			SourceResults: len(results),
			Latency:       time.Since(started),
		})
	}
	if len(sourceErrs) > 0 && len(sourceErrs) == totalRows && totalCandidates == 0 {
		return detail, read.AllSourcesFailed(sourceErrs)
	}
	return detail, nil
}

var _ pipeline.Stage[*read.ReadState] = (*CandidateFanout)(nil)
