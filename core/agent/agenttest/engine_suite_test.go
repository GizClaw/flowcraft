package agenttest_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
)

// fakeEngine is the minimum-correct agent.Engine for the contract
// suite: a single select between ctx, interrupt, and an immediate
// completion path. It does not support resume, so it must reject any
// non-nil Run.ResumeFrom with NotAvailable.
type fakeEngine struct{}

func (fakeEngine) Execute(
	ctx context.Context,
	run agent.Run,
	host agent.Host,
	board *agent.Board,
) (*agent.Board, error) {
	if run.ResumeFrom != nil {
		if run.ResumeFrom.ExecID != run.RunID {
			return board, errdefs.Validationf(
				"engine: ResumeFrom.ExecID %q != Run.RunID %q",
				run.ResumeFrom.ExecID, run.RunID,
			)
		}
		return board, errdefs.NotAvailablef("fakeEngine: resume not supported")
	}

	select {
	case <-ctx.Done():
		return board, ctx.Err()
	case intr := <-host.Interrupts():
		return board, agent.Interrupted(intr)
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
	run agent.Run,
	host agent.Host,
	board *agent.Board,
) (*agent.Board, error) {
	if run.ResumeFrom != nil && run.ResumeFrom.ExecID != run.RunID {
		return board, errdefs.Validationf(
			"engine: ResumeFrom.ExecID %q != Run.RunID %q",
			run.ResumeFrom.ExecID, run.RunID,
		)
	}
	select {
	case <-ctx.Done():
		return board, ctx.Err()
	case intr := <-host.Interrupts():
		return board, agent.Interrupted(intr)
	default:
		return board, nil
	}
}

// fakeUsageReportingEngine reports one usage delta before completing,
// so the suite's budget-exceeded subtest can observe the signal. It
// is otherwise identical to fakeEngine.
type fakeUsageReportingEngine struct{}

func (fakeUsageReportingEngine) Execute(
	ctx context.Context,
	run agent.Run,
	host agent.Host,
	board *agent.Board,
) (*agent.Board, error) {
	if run.ResumeFrom != nil {
		if run.ResumeFrom.ExecID != run.RunID {
			return board, errdefs.Validationf(
				"engine: ResumeFrom.ExecID %q != Run.RunID %q",
				run.ResumeFrom.ExecID, run.RunID,
			)
		}
		return board, errdefs.NotAvailablef("fakeUsageReportingEngine: resume not supported")
	}

	if err := host.ReportUsage(ctx, inference.Usage{InputTokens: 1}); err != nil {
		return board, err
	}

	select {
	case <-ctx.Done():
		return board, ctx.Err()
	case intr := <-host.Interrupts():
		return board, agent.Interrupted(intr)
	default:
		return board, nil
	}
}

// TestSuite_FakeEngine pins down that the contract suite passes
// against a minimal correct agent. If this ever fails, the suite
// itself has drifted from the agent.Engine contract.
func TestSuite_FakeEngine(t *testing.T) {
	agenttest.EngineSuite(t, func() (agent.Engine, agenttest.Capabilities) {
		return fakeEngine{}, agenttest.Capabilities{}
	})
}

// TestSuite_FakeResumableEngine pins down the resume-supported branch
// of the suite.
func TestSuite_FakeResumableEngine(t *testing.T) {
	agenttest.EngineSuite(t, func() (agent.Engine, agenttest.Capabilities) {
		return fakeResumableEngine{}, agenttest.Capabilities{SupportsResume: true}
	})
}

// TestSuite_FakeUsageReportingEngine pins down the budget-exceeded
// branch: the fake reports usage, observes the injected budget error,
// and returns it.
func TestSuite_FakeUsageReportingEngine(t *testing.T) {
	agenttest.EngineSuite(t, func() (agent.Engine, agenttest.Capabilities) {
		return fakeUsageReportingEngine{}, agenttest.Capabilities{}
	})
}
