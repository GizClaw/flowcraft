package runner

import (
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// engine.go isolates the engine.Engine-specific glue for [Runner]. The
// implementation of Execute itself lives in runner.go alongside the
// other execution paths so the option-assembly logic stays in one
// place; this file only carries the policies that diverge from a plain
// graph dispatch — chiefly resume classification, which the engine.Engine
// contract requires us to surface as typed errors rather than silently
// restarting.

// Compile-time check: Runner satisfies engine.Engine. If this assertion
// breaks the agent runtime would refuse to drive Runner instances and
// the v0.3.0 deprecation plan for executor.Executor would regress —
// keep it here so a signature drift fails the build instead of silently
// losing the integration.
var _ engine.Engine = (*Runner)(nil)

// classifyResume turns a non-nil run.ResumeFrom into the right error
// per the engine.Engine contract documented in sdk/engine/engine.go:
//
//   - Foreign ExecID → errdefs.Validation. Supplying a checkpoint that
//     belongs to a different run is "trying to fork", not "trying to
//     resume", and the contract treats that as a programmer error so
//     callers do not accidentally cross-pollinate state.
//
//   - Same ExecID but resume is not implemented → errdefs.NotAvailable.
//     The graph runner does not yet replay from a checkpoint; returning
//     NotAvailable lets hosts route to a fallback (start fresh, surface
//     to the user, …) without confusing it with a hard failure.
//
// Returning nil here would imply the runner accepted the resume —
// callers should never reach that branch today.
func classifyResume(run engine.Run) error {
	cp := run.ResumeFrom
	if cp == nil {
		return nil
	}
	if cp.ExecID != "" && cp.ExecID != run.ID {
		return errdefs.Validationf(
			"graph runner: ResumeFrom.ExecID %q does not match Run.ID %q "+
				"(use a fresh Run.ID to fork instead of resume)",
			cp.ExecID, run.ID)
	}
	return errdefs.NotAvailablef(
		"graph runner: resume from checkpoint is not implemented (Run.ID=%q)",
		run.ID)
}
