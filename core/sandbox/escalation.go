package sandbox

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// This file owns the escalation path: what an OS sandbox refusal looks
// like in captured output, the question the user is asked about it,
// and the one-shot front end that acts on the answer.
//
// The approval gate ([WithApproval]) decides whether a command may
// *run*; it deliberately never widens the policy the command runs
// under, so a command that was allowed and then refused by
// seatbelt/bwrap/windows fails like any other command. Escalation is
// the second, explicit question for exactly that case: the user is
// asked whether this one command may leave the confine, and the
// answer is an ordinary decision the front end can act on.

// EscalationRequest describes one confined attempt the OS sandbox
// refused. It is rendered into the user prompt; Command is the command
// line rebuilt from the spawn argv (what the user sees, including any
// shell wrapper), while Rule is the normalized token prefix persisted
// for a "remember" answer. The two differ for shell-wrapped calls
// ("sh -c 'pip install x'" vs "pip install x").
type EscalationRequest struct {
	Command string
	Rule    string
	Reason  string
	Detail  string
}

// EscalationDecision is the user's answer to an [EscalationRequest].
// Remember asks for an "always run this command without the sandbox"
// rule; persisting (and reading back) that rule belongs to the
// Escalator and [EscalationRules], not to this package.
type EscalationDecision struct {
	Allow    bool
	Remember bool
}

// Escalator asks the user to widen the boundary for one refused
// command. A host implements it over its own prompt protocol; without
// a wired escalator (headless deployments, tests) a refusal stays a
// plain command failure.
type Escalator interface {
	Escalate(
		ctx context.Context, req EscalationRequest,
	) (EscalationDecision, error)
}

// EscalationRules is the read side of the persisted escalation rules.
// The front end consults it before the first attempt so a remembered
// rule skips the confined attempt (and its doomed first failure)
// entirely.
type EscalationRules interface {
	EscalatedAllowed(req ExecRequest) bool
}

// EscalationGate reports whether the escalation retry may be offered
// for the call in ctx at all. It is the session-mode owner's half of
// the policy: a session that already runs unconfined has no confine
// to leave, and a read-only session must not trade its workspace
// read-only guarantee for a per-command approval. A nil gate means
// no: a retry is offered only where a gate says so.
type EscalationGate interface {
	EscalationAvailable(ctx context.Context) bool
}

// EscalationGateFunc lets a plain closure act as an [EscalationGate].
type EscalationGateFunc func(ctx context.Context) bool

// EscalationAvailable implements EscalationGate.
func (f EscalationGateFunc) EscalationAvailable(ctx context.Context) bool {
	return f(ctx)
}

// RuleFor renders one spawn's argv as the rule string the escalation
// store matches against: shell wrappers ("sh -c ...") are unwrapped
// exactly like the allowlist path does, so a remembered rule describes
// what the user actually asked to run. Unrecognized or complex scripts
// stay wrapped (see [NormaliseExec]) and will not match a rule.
func RuleFor(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return strings.Join(NormaliseExec(ExecRequest{
		Command: argv[0],
		Args:    argv[1:],
	}), " ")
}

// denialMarkers are the stderr fragments the OS backends and the
// shells in front of them leave behind when a syscall is refused.
// macOS seatbelt denies a write with EPERM ("Operation not
// permitted"), bwrap leaves the rest of the filesystem on a read-only
// bind (EROFS), and the Windows write-confinement token fails with
// access denied, phrased differently by each shell: cmd says "Access
// is denied", while PowerShell reports the .NET error ("Access to the
// path 'X' is denied", "UnauthorizedAccessException",
// "PermissionDenied").
//
// "permission denied" is deliberately absent: it is the ordinary
// EACCES of unrelated file-mode problems, and prompting to leave the
// sandbox for those would train users to approve noise.
var denialMarkers = []string{
	"operation not permitted",
	"read-only file system",
	"access is denied",
	"access to the path",
	"unauthorizedaccessexception",
	"permissiondenied",
	"deny file-write",
}

// quickRejectExitCodes are the conventional "cannot execute" statuses
// (not executable, command not found). They never mean the sandbox
// refused a syscall.
//
// Exit code 2 is deliberately absent even though it is the other
// conventional usage-error status: dash returns 2 when a redirection
// fails, which is exactly what a refused write looks like
// ("/bin/sh: 1: cannot create ...: Read-only file system"). The marker
// text, not the exit code, is what separates a refusal from an
// ordinary failure.
var quickRejectExitCodes = map[int]bool{126: true, 127: true}

// Denied reports whether res looks like the OS sandbox refused the
// command, with a short reason for the user prompt.
//
// Like every command predicate in this package this is a tripwire,
// not a wall: a false positive only produces a prompt the user can
// decline, and the OS backend stays the enforcement point. It exists
// because a refusal is otherwise indistinguishable from a command
// that genuinely lacked permission.
func Denied(res *ExecResult) (string, bool) {
	if res == nil || res.ExitCode == 0 || quickRejectExitCodes[res.ExitCode] {
		return "", false
	}
	haystack := strings.ToLower(res.Stderr)
	if haystack == "" {
		haystack = strings.ToLower(res.Stdout)
	}
	for _, marker := range denialMarkers {
		if !strings.Contains(haystack, marker) {
			continue
		}
		return "the sandbox refused the command (" + marker + ")", true
	}
	return "", false
}

// DenialExcerpt picks the stderr line that carries the refusal, so the
// escalation prompt can show the concrete error instead of the whole
// captured output. It falls back to the first non-empty stderr line.
func DenialExcerpt(res *ExecResult) string {
	const maxLen = 400
	if res == nil {
		return ""
	}
	for _, line := range strings.Split(res.Stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !isDenialLine(line) {
			continue
		}
		return truncateDenialLine(line, maxLen)
	}
	for _, line := range strings.Split(res.Stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateDenialLine(line, maxLen)
		}
	}
	return ""
}

func isDenialLine(line string) bool {
	lower := strings.ToLower(line)
	for _, marker := range denialMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// truncateDenialLine keeps an over-long line readable in a prompt; the
// excerpt is display-only.
func truncateDenialLine(line string, maxLen int) string {
	if len(line) <= maxLen {
		return line
	}
	return line[:maxLen] + "..."
}

// Escalation is the one-shot front end that adds the approved-retry
// path to a confined chain: a command runs under Confined; when the
// OS sandbox refuses it, the gate is consulted, the user is asked
// through Escalate, and an approved answer re-runs the whole command
// through Unconfined exactly once.
//
// Escalation is deliberately not a [Runner]. The one-shot [Exec] is a
// derived view over Start, and a Start-level wrapper cannot see the
// completed result a refusal is recognized in; the retry therefore
// lives at the Exec level, next to the call that produced the result.
// Interactive sessions do not take this path either: a session is a
// persistent command channel whose refusals surface over many reads,
// and restarting one is not what the user approved.
//
// Everything optional fails closed: a nil Escalate or Gate never
// offers a retry, a nil Rules remembers nothing, and a prompt that
// errors keeps the confined result. Both chains are required.
type Escalation struct {
	// Confined is where commands run by default: the approval /
	// defaults / OS-backend chain. Required.
	Confined Runner
	// Unconfined is the retry target: the same command outside the
	// confine, with byte-identical [ExecOptions]. Required.
	Unconfined Runner
	// Escalate is the ask channel for one refused command. Nil
	// disables escalation: refusals stay ordinary command failures.
	Escalate Escalator
	// Rules is the read side of remembered "run this outside the
	// sandbox" rules. A covered call skips the confined attempt and
	// runs through Unconfined directly. Nil remembers nothing.
	Rules EscalationRules
	// Gate is the session-mode check behind every offer. Nil means
	// no: hosts that model permission modes wire it explicitly (see
	// [EscalationGate]).
	Gate EscalationGate
}

// EscalationOutcome reports what one [Escalation.Exec] run did.
type EscalationOutcome struct {
	// Refused reports the confined attempt looked like an OS sandbox
	// refusal.
	Refused bool
	// Approved reports the command ran outside the confine because an
	// approval covered it: a fresh user answer or a remembered rule.
	Approved bool
	// Remembered reports the approval came from (or was stored as) a
	// remembered rule. The store itself belongs to the Escalator.
	Remembered bool
}

// Exec runs one command through the escalation flow and reports the
// result the caller should surface. The result is the confined
// attempt's unless an approval re-ran the command through Unconfined,
// in which case the retry's result stands, including its failure,
// which is the truth the user asked for. A prompt error never
// discards the confined result (fail closed); errors from either
// attempt are returned as-is.
func (e *Escalation) Exec(
	ctx context.Context,
	cmd string,
	args []string,
	opts ExecOptions,
) (*ExecResult, EscalationOutcome, error) {
	var outcome EscalationOutcome
	if e == nil || e.Confined == nil || e.Unconfined == nil {
		return nil, outcome, errdefs.Validationf(
			"sandbox: escalation requires confined and unconfined runners")
	}

	argv := append([]string{cmd}, args...)
	available := e.Gate != nil && e.Gate.EscalationAvailable(ctx)

	// A remembered rule skips the confined attempt entirely, but only
	// where a retry would be offerable at all: the read-only
	// mode must not be traded even for a rule the user approved
	// earlier.
	if available && e.Rules != nil && e.Rules.EscalatedAllowed(
		ExecRequest{Command: cmd, Args: args, Opts: opts},
	) {
		res, err := Exec(ctx, e.Unconfined, cmd, args, opts)
		outcome.Approved = true
		outcome.Remembered = true
		return res, outcome, err
	}

	res, err := Exec(ctx, e.Confined, cmd, args, opts)
	if err != nil {
		return res, outcome, err
	}
	reason, refused := Denied(res)
	if !refused {
		return res, outcome, nil
	}
	outcome.Refused = true
	if !available || e.Escalate == nil {
		return res, outcome, nil
	}
	decision, err := e.Escalate.Escalate(ctx, EscalationRequest{
		Command: strings.Join(argv, " "),
		Rule:    RuleFor(argv),
		Reason:  reason,
		Detail:  DenialExcerpt(res),
	})
	if err != nil {
		// Fail closed: the refusal stands unless the user could be
		// asked. The confined result is still the truth on the
		// ground.
		telemetry.WarnErr(ctx,
			"sandbox: escalation prompt failed; keeping the confined result",
			err)
		return res, outcome, nil
	}
	if !decision.Allow {
		return res, outcome, nil
	}
	outcome.Approved = true
	outcome.Remembered = decision.Remember
	retried, err := Exec(ctx, e.Unconfined, cmd, args, opts)
	return retried, outcome, err
}
