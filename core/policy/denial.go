package policy

import (
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Check names the rule that refused a request. A denial always carries one, so a
// caller can count refusals by rule without parsing an error string.
type Check string

const (
	// CheckClamp is a request that asked for more than the ceiling allows.
	CheckClamp Check = "clamp"
	// CheckVisibility is a tool the session may not see or call.
	CheckVisibility Check = "visibility"
	// CheckWrites is a path outside the session's writable set.
	CheckWrites Check = "writes"
	// CheckModels is a model outside the session's model set.
	CheckModels Check = "models"
	// CheckBudget is a definition or spend budget that was exceeded.
	CheckBudget Check = "budget"
	// CheckApproval is a command that a host's approval rule refused.
	//
	// It is here rather than in the host because a refusal always carries the rule that refused:
	// a dashboard counting denials by check should not need a second vocabulary for the one rule
	// that happens to involve a person. What this package still does not do is decide it.
	CheckApproval Check = "approval"
	// CheckEscalation is a widening outside what the deployment declared: a grant
	// that would drop what the session already had, one that reaches beyond the
	// escalation, or a session that has spent its widening budget.
	//
	// It is here beside [CheckApproval] for the same reason: the host decides
	// whether to ask, and the rule that refuses is this package's, so a refusal
	// names the rule without a second vocabulary.
	CheckEscalation Check = "escalation"
)

// Checks lists every rule that can refuse, in declaration order.
//
// It exists because the recording layer and the guard test both need the whole
// set: a check added without being listed would be counted under no name, and a
// dashboard would report a rule that never fires.
func Checks() []Check {
	return []Check{
		CheckClamp,
		CheckVisibility,
		CheckWrites,
		CheckModels,
		CheckBudget,
		CheckApproval,
		CheckEscalation,
	}
}

// Denial is a policy refusal.
//
// It is a value rather than a log line: this package does not emit telemetry, so
// the layer that turns a denial into a span event, a counter and a model-facing
// message is the one that called into the policy — dispatch for a tool call, the
// host for a session, the runtime for a model subset.
type Denial struct {
	// Check is the rule that refused.
	Check Check
	// Reason is a single-line, model-facing explanation. It must not carry
	// anything the caller would not want a model to read: it is rendered back
	// to the agent so it can correct itself.
	Reason string
	// Cause is the failure behind a refusal, when there was one: an approver that errored, a
	// store that could not answer. It is deliberately not part of [Denial.Error] and must never
	// be copied into Reason — the difference between "the policy refused" and "the policy could
	// not be consulted" is what a caller needs to decide whether to retry, and it is exactly the
	// detail that must not reach a prompt.
	Cause error
}

func (d *Denial) Error() string {
	return fmt.Sprintf("policy denied (%s): %s", d.Check, d.Reason)
}

// Unwrap exposes the classification so errdefs.IsPolicyDenied keeps working
// through the typed value.
func (d *Denial) Unwrap() error {
	return errdefs.PolicyDeniedf("%s: %s", d.Check, d.Reason)
}

// AsDenial reports whether err is, or wraps, a policy denial.
func AsDenial(err error) (*Denial, bool) {
	var d *Denial
	if errors.As(err, &d) {
		return d, true
	}
	return nil, false
}

// deny builds a denial. Keeping construction in one function is what makes the
// checks enumerable — see [Checks] — because every refusal in this package goes
// through it.
func deny(check Check, format string, args ...any) error {
	return &Denial{Check: check, Reason: fmt.Sprintf(format, args...)}
}
