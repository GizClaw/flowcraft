package stages

import (
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// SourceProvider returns the live source list (memory.sources may be
// replaced after New, e.g. in tests).
type SourceProvider func() []port.Source

// SourceFanout queries each source in planner order for every
// sub-scope (len==1 today).
type SourceFanout struct {
	sources SourceProvider
}

// NewSourceFanout constructs a SourceFanout stage.
func NewSourceFanout(sources SourceProvider) *SourceFanout {
	return &SourceFanout{sources: sources}
}

// Name implements pipeline.Stage.
func (SourceFanout) Name() string { return "source_fanout" }

// Run implements pipeline.Stage.
func (s *SourceFanout) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	srcs := s.sources()
	byName := make(map[string]port.Source, len(srcs))
	for _, src := range srcs {
		byName[src.Name()] = src
	}

	var detail diagnostic.SourceFanoutDetail
	var sourceErrs []error
	totalCandidates := 0

	for i := range state.SubScopeStates {
		sub := &state.SubScopeStates[i]
		if sub.Plan == nil {
			continue
		}
		plan := *sub.Plan
		results := make([]domain.SourceResult, 0, len(plan.SourceOrder))
		for _, name := range plan.SourceOrder {
			src, ok := byName[name]
			if !ok {
				continue
			}
			res := src.Query(ctx, plan)
			results = append(results, res)
			row := diagnostic.SourceResult{
				Lens:       res.Source,
				Candidates: len(res.Candidates),
				Latency:    res.Latency,
			}
			if res.Err != nil {
				row.Err = res.Err.Error()
				sourceErrs = append(sourceErrs, fmt.Errorf("%s: %w", res.Source, res.Err))
			}
			detail.Results = append(detail.Results, row)
			totalCandidates += len(res.Candidates)

			if state.Trace != nil {
				st := domain.SourceTrace{
					Source:    res.Source,
					Budget:    plan.SourceBudgets[res.Source],
					Returned:  len(res.Candidates),
					Truncated: res.Truncated,
					Latency:   res.Latency,
				}
				if res.Err != nil {
					st.Err = res.Err.Error()
				}
				state.Trace.Sources = append(state.Trace.Sources, st)
			}
		}
		sub.SourceResults = results
	}

	if len(sourceErrs) > 0 && len(sourceErrs) == len(detail.Results) && totalCandidates == 0 {
		return detail, fmt.Errorf("recall.Recall: all sources failed: %w", errors.Join(sourceErrs...))
	}
	return detail, nil
}

var _ pipeline.Stage[*read.ReadState] = (*SourceFanout)(nil)
