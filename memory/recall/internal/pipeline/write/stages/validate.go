// Package stages assembles the write-flow pipeline's ordered Stage
// list. One stage per file keeps each concrete responsibility easy to
// review.
//
// Stages mutate the shared *write.WriteState; they do not call
// telemetry hooks directly. The pipeline framework owns
// StageDiagnostic emission; Compensated events fired during
// compensation route through the same OnStage rail.
package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Validate is the first stage of the write pipeline. It rejects
// permanently malformed inputs (missing RuntimeID) before any side
// effect happens. Validation failures return errdefs.Validation so
// HTTP/gRPC adapters map to 400 without text matching.
type Validate struct{}

// NewValidate returns a Validate stage instance. The stage is
// stateless so a single value is safe to share across runs.
func NewValidate() *Validate { return &Validate{} }

// Name implements pipeline.Stage.
func (Validate) Name() string { return "validate" }

// Run implements pipeline.Stage. The Detail carries rejection summary fields so
// diagnostics consumers can attribute "input turn count" and "permanent reject"
// counts without a second pass.
//
// Per-fact validation rules:
//
//   - Every element of TemporalFact.Supersedes must be a non-empty
//     ID. len > 1 is allowed for explicit 1:N supersede. The resolver
//     later validates that each ID exists in the store; this stage
//     only catches structural mistakes (caller passed an empty string)
//     before any side effect runs.
func (Validate) Run(_ context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	detail := diagnostic.ValidateDetail{InputTurns: len(state.Turns)}
	if state.Scope.RuntimeID == "" {
		detail.Rejected = 1
		detail.RejectReason = "scope.runtime_id is required"
		state.FailedStage = "validate"
		return detail, errdefs.Validationf("recall.Save: scope.runtime_id is required")
	}
	for i, f := range state.Facts {
		// KindEpisode is produced exclusively by the async episode lane
		// (build_episode stage stamps Origin.Kind=episode). Reject caller-
		// supplied episode facts at the synchronous Save boundary so raw
		// episodes only flow through the dedicated episode projection path.
		if f.Kind == domain.KindEpisode {
			detail.Rejected = 1
			detail.RejectReason = "KindEpisode is reserved for the async episode lane"
			state.FailedStage = "validate"
			return detail, errdefs.Validationf("recall.Save: facts[%d].Kind=KindEpisode is reserved for WriteModeAsyncSemantic", i)
		}
		for j, prior := range f.Supersedes {
			if prior == "" {
				detail.Rejected = 1
				detail.RejectReason = "supersedes contains empty id"
				state.FailedStage = "validate"
				return detail, errdefs.Validationf("recall.Save: facts[%d].Supersedes[%d] is empty", i, j)
			}
		}
	}
	return detail, nil
}

// compile-time assertion: Validate is a pipeline Stage for the
// write flow.
var _ pipeline.Stage[*write.WriteState] = (*Validate)(nil)
