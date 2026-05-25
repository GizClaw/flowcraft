package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// FusionCapFunc computes the per-source fusion pool cap from the
// plan's final hit cap (sdk/recall.fusionCandidateCap today).
type FusionCapFunc func(finalCap int) int

// Fuse runs weighted RRF per sub-scope.
type Fuse struct {
	fuser   port.Fuser
	opts    port.FusionOptions
	capFunc FusionCapFunc
}

// NewFuse constructs a Fuse stage.
func NewFuse(fuser port.Fuser, opts port.FusionOptions, capFunc FusionCapFunc) *Fuse {
	return &Fuse{fuser: fuser, opts: opts, capFunc: capFunc}
}

// Name implements pipeline.Stage.
func (Fuse) Name() string { return "fuse" }

// Run implements pipeline.Stage.
func (s *Fuse) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	opts := s.opts
	if opts.TotalCap == 0 && state.Plan != nil && s.capFunc != nil {
		opts.TotalCap = s.capFunc(state.Plan.TotalCap)
	}
	if state.Plan != nil && (state.Plan.Intent.Features.HasTimeSignal() || state.Plan.Intent.Features.NumericIntent) {
		opts.SourceFloors = mergeSourceFloors(opts.SourceFloors, map[string]int{
			planner.SourceRetrieval: 5,
			planner.SourceTimeline:  3,
		})
	}
	var (
		inputCount int
		dropCount  int
	)
	for i := range state.SubScopeStates {
		sub := &state.SubScopeStates[i]
		for _, res := range sub.SourceResults {
			inputCount += len(res.Candidates)
		}
		fused, drops, err := s.fuser.Fuse(ctx, sub.SourceResults, opts)
		if err != nil {
			return diagnostic.FuseDetail{InputCount: inputCount}, err
		}
		sub.Fused = fused
		sub.FusionDrops = drops
		dropCount += len(drops)
	}
	return diagnostic.FuseDetail{
		InputCount:     inputCount,
		AfterDedup:     inputCount - dropCount,
		DroppedByDedup: dropCount,
	}, nil
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

var _ pipeline.Stage[*read.ReadState] = (*Fuse)(nil)
