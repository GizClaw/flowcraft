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
)

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
// five checks enumerable: every refusal in this package goes through it.
func deny(check Check, format string, args ...any) error {
	return &Denial{Check: check, Reason: fmt.Sprintf(format, args...)}
}
