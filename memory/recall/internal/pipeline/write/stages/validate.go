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

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
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
//
// Per-fact validation rules:
//
//   - Every element of TemporalFact.Supersedes must be a non-empty
//     ID. len > 1 is allowed (D1, 2026-05-21: explicit 1:N
//     supersede). The resolver later validates that each ID exists
//     in the store; this stage only catches structural mistakes
//     (caller passed an empty string) before any side effect runs.
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
		// (build_episode stage stamps Origin.Kind=episode). Accepting a
		// caller-supplied episode fact in the sync path would route it
		// through ProjectRequired, which doesn't honour
		// KindFilteredProjection — episodes would land in retrieval /
		// entity / profile and trigger embedder calls. Reject at the
		// boundary instead.
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
