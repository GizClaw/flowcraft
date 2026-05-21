// Package stages assembles the write-flow pipeline's ordered Stage
// list. One stage per file mirrors plan §3.B.2 C1-C9 so reviewers
// can map each commit to a single concrete responsibility.
//
// Stages mutate the shared *write.WriteState; they do not call
// telemetry hooks directly. The pipeline framework owns
// StageDiagnostic emission; Compensated events fired during
// compensation route through the same OnStage rail (Phase E.3:
// single-rail surface).
package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
)

// Validate is the first stage of the write pipeline. It rejects
// permanently malformed inputs (missing RuntimeID) before any side
// effect happens. The legacy runSave performed the same check inline
// and returned an errdefs.Validation error — we preserve that
// classification so HTTP/gRPC shims still map to 400 without text
// matching.
type Validate struct{}

// NewValidate returns a Validate stage instance. The stage is
// stateless so a single value is safe to share across runs.
func NewValidate() *Validate { return &Validate{} }

// Name implements pipeline.Stage.
func (Validate) Name() string { return "validate" }

// Run implements pipeline.Stage. The Detail mirrors the legacy
// rejection summary so diagnostics consumers can attribute "input
// turn count" and "permanent reject" counts without a second pass.
func (Validate) Run(_ context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	detail := diagnostic.ValidateDetail{InputTurns: len(state.Turns)}
	if state.Scope.RuntimeID == "" {
		detail.Rejected = 1
		detail.RejectReason = "scope.runtime_id is required"
		state.FailedStage = "validate"
		return detail, errdefs.Validationf("recall.Save: scope.runtime_id is required")
	}
	return detail, nil
}

// compile-time assertion: Validate is a pipeline Stage for the
// write flow.
var _ pipeline.Stage[*write.WriteState] = (*Validate)(nil)
