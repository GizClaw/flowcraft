package enginetest_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// fakeEngine is the minimum-correct engine.Engine for the contract
// suite: a single select between ctx, interrupt, and an immediate
// completion path. It does not support resume, so it must reject any
// non-nil Run.ResumeFrom with NotAvailable.
type fakeEngine struct{}

func (fakeEngine) Execute(
	ctx context.Context,
	run engine.Run,
	host engine.Host,
	board *engine.Board,
) (*engine.Board, error) {
	if run.ResumeFrom != nil {
		if run.ResumeFrom.ExecID != run.ID {
			return board, errdefs.Validationf(
				"engine: ResumeFrom.ExecID %q != Run.ID %q",
				run.ResumeFrom.ExecID, run.ID,
			)
		}
		return board, errdefs.NotAvailablef("fakeEngine: resume not supported")
	}

	select {
	case <-ctx.Done():
		return board, ctx.Err()
	case intr := <-host.Interrupts():
		return board, engine.Interrupted(intr)
	default:
		return board, nil
	}
}

// fakeResumableEngine is identical to fakeEngine but advertises
// resume support and silently accepts a matching-ExecID checkpoint.
// The contract suite verifies the matching-ExecID path completes
// cleanly when SupportsResume == true.
type fakeResumableEngine struct{}

func (fakeResumableEngine) Execute(
	ctx context.Context,
	run engine.Run,
	host engine.Host,
	board *engine.Board,
) (*engine.Board, error) {
	if run.ResumeFrom != nil && run.ResumeFrom.ExecID != run.ID {
		return board, errdefs.Validationf(
			"engine: ResumeFrom.ExecID %q != Run.ID %q",
			run.ResumeFrom.ExecID, run.ID,
		)
	}
	select {
	case <-ctx.Done():
		return board, ctx.Err()
	case intr := <-host.Interrupts():
		return board, engine.Interrupted(intr)
	default:
		return board, nil
	}
}

// TestSuite_FakeEngine pins down that the contract suite passes
// against a minimal correct engine. If this ever fails, the suite
// itself has drifted from the engine.Engine contract.
func TestSuite_FakeEngine(t *testing.T) {
	enginetest.RunSuite(t, func() (engine.Engine, enginetest.Capabilities) {
		return fakeEngine{}, enginetest.Capabilities{}
	})
}

// TestSuite_FakeResumableEngine pins down the resume-supported branch
// of the suite.
func TestSuite_FakeResumableEngine(t *testing.T) {
	enginetest.RunSuite(t, func() (engine.Engine, enginetest.Capabilities) {
		return fakeResumableEngine{}, enginetest.Capabilities{SupportsResume: true}
	})
}
