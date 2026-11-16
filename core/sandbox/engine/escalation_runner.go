package engine

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// EscalationRunner exposes an [Escalation] as a [sandbox.Runner], so a host can
// hand the composed chain — confined runner plus approved retry — to anything
// that takes a runner, including a runtime's exec tools.
//
// The split is the same one [Escalation] documents: the retry lives at the
// one-shot level, so Exec is the escalation flow and every session call
// forwards to Escalation.Confined. An interactive session is refused over many
// reads, not one result, and restarting one behind the user's back is not what
// an approval covers — sessions never escalate.
//
// Ownership: the runner forwards Confined (the runtime that built the chain
// closes it through this value, exactly once) and releases Unconfined itself,
// because the host built that chain inside the decorator and the runtime never
// saw it.
type EscalationRunner struct {
	*Forwarder
	escalation *Escalation
	observe    func(EscalationOutcome)
}

// EscalationRunnerOption adjusts an [EscalationRunner] at construction.
type EscalationRunnerOption func(*EscalationRunner)

// WithEscalationObserver installs a callback that sees the outcome of every
// one-shot call, including a plain confined success (the zero outcome). It is
// where a host records that a command was refused, approved or ran under a
// remembered rule; the runner works without one.
func WithEscalationObserver(fn func(EscalationOutcome)) EscalationRunnerOption {
	return func(r *EscalationRunner) { r.observe = fn }
}

// NewEscalationRunner wires esc into a runner. Both chains are required: a
// confined chain with no retry target can only ever fail closed, and a retry
// target with no confine is not an escalation.
//
// The escalation is copied at construction: its fields are exported and mutable,
// so a host that rebuilt Escalation.Confined afterwards would split the chain
// Start forwards to from the one Exec re-runs, and leak the runner it swapped
// in.
func NewEscalationRunner(esc *Escalation, opts ...EscalationRunnerOption) (*EscalationRunner, error) {
	if esc == nil {
		return nil, errdefs.Validationf("sandbox: escalation runner requires an escalation")
	}
	if esc.Confined == nil || esc.Unconfined == nil {
		return nil, errdefs.Validationf(
			"sandbox: escalation requires confined and unconfined runners")
	}
	copied := *esc
	forwarder, err := NewForwarder(copied.Confined)
	if err != nil {
		return nil, err
	}
	runner := &EscalationRunner{Forwarder: forwarder, escalation: &copied}
	for _, opt := range opts {
		opt(runner)
	}
	return runner, nil
}

// Exec implements [sandbox.Executor]: the command runs through the confined
// chain, and an approved refusal re-runs it through Unconfined. The outcome is
// reported to the observer when one is installed.
func (r *EscalationRunner) Exec(
	ctx context.Context,
	cmd string,
	args []string,
	opts sandbox.ExecOptions,
) (*sandbox.ExecResult, error) {
	result, outcome, err := r.escalation.Exec(ctx, cmd, args, opts)
	if r.observe != nil {
		r.observe(outcome)
	}
	return result, err
}

// Close implements [sandbox.Runner]: it releases the retry chain this value
// owns, then forwards to Confined. Both are idempotent, and the runtime closes
// this value once.
func (r *EscalationRunner) Close() error {
	var first error
	if r.escalation.Unconfined != nil {
		if err := r.escalation.Unconfined.Close(); err != nil {
			first = err
		}
	}
	if err := r.Forwarder.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

var (
	_ sandbox.Runner   = (*EscalationRunner)(nil)
	_ sandbox.Executor = (*EscalationRunner)(nil)
)
