package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
)

// OriginStamp stamps SemanticDerivationOrigin onto every resolved
// fact before append. The async semantic worker sets the origin on
// WriteState; the synchronous path leaves it zero and skips.
type OriginStamp struct{}

// NewOriginStamp constructs the stage.
func NewOriginStamp() *OriginStamp { return &OriginStamp{} }

// Name implements pipeline.Stage.
func (OriginStamp) Name() string { return "origin_stamp" }

// Skip implements pipeline.Conditional.
func (OriginStamp) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if state == nil || state.SemanticDerivationOrigin.IsZero() || !state.HasWork() {
		return true, diagnostic.OriginStampDetail{}
	}
	return false, nil
}

// Run implements pipeline.Stage.
func (OriginStamp) Run(_ context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	origin := state.SemanticDerivationOrigin
	for i := range state.Resolution.Facts {
		state.Resolution.Facts[i].Origin = origin
	}
	return diagnostic.OriginStampDetail{
		AsyncRequestID: state.AsyncRequestID,
		Facts:          len(state.Resolution.Facts),
	}, nil
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*OriginStamp)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*OriginStamp)(nil)
)
