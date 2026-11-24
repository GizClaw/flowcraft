package host

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Approval is the host's rule for starting a command.
//
// It is a product feature, not a sandbox capability: whether a person (or a list) has to agree before
// a command runs is a decision about this deployment, and the platform's job is only to ask at the
// right place. That place is [sandbox.SessionStarter] — one method — and not [sandbox.Runner], whose
// five methods would leave every decorator forwarding the optional capabilities it does not care
// about.
//
// Two rules, applied in this order: a command the [Whitelist] covers runs without asking, and
// everything else is put to the [Approver]. A policy with neither refuses every command, which is
// what "I have not decided" must mean; a process with no policy at all installs the starter
// undecorated, so approval stays opt-in.
type Approval struct {
	// Approver decides the commands the whitelist does not cover. Nil means nothing is covered
	// beyond the whitelist.
	Approver Approver
	// Whitelist holds the commands that run without approval.
	Whitelist Whitelist
}

// Approver decides one command.
//
// It is called on the turn's goroutine and must return promptly: a turn is blocked for as long as it
// runs, which is what an operator answering a prompt looks like from the model's side.
type Approver interface {
	// Approve reports nil when the command may run, or a [*policy.Denial] when it may not.
	Approve(ctx context.Context, request ApprovalRequest) error
}

// ApprovalRequest is what an approver is told: enough to judge the command, and nothing about the
// session's internals.
type ApprovalRequest struct {
	// Agent and SessionID identify who is asking.
	Agent     string
	SessionID string
	// Argv is the command line, already resolved into words.
	Argv []string
	// WorkDir is the absolute directory it would run in.
	WorkDir string
	// TTY reports whether the command asks for a terminal, which is the difference between a
	// batch command and an interactive one.
	TTY bool
}

// Whitelist is the commands that run without approval, matched by argv prefix.
//
// An entry is a decision, not a safety net: "git" covers `git push --force` as well as `git status`,
// so a deployment that whitelists an interpreter or a read-write tool has whitelisted everything that
// tool can do. Entries are compared word by word against the front of the command line; an empty
// entry matches nothing rather than everything.
type Whitelist [][]string

// Covers reports whether argv starts with one of the entries.
func (w Whitelist) Covers(argv []string) bool {
	for _, entry := range w {
		if len(entry) == 0 || len(entry) > len(argv) {
			continue
		}
		matched := true
		for i, word := range entry {
			if argv[i] != word {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// approvalStarter is the decorator the session's process capability carries when a policy exists.
type approvalStarter struct {
	next    sandbox.SessionStarter
	policy  Approval
	agent   string
	session string
}

// Start refuses the command the policy does not cover before anything is spawned.
func (s approvalStarter) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if s.policy.Whitelist.Covers(spec.Argv) {
		return s.next.Start(ctx, spec)
	}
	request := ApprovalRequest{
		Agent:     s.agent,
		SessionID: s.session,
		Argv:      append([]string(nil), spec.Argv...),
		WorkDir:   spec.Opts.WorkDir,
		TTY:       spec.TTY,
	}
	if s.policy.Approver == nil {
		return nil, refusal(ctx, s.agent, "no approver is configured and the command is not whitelisted")
	}
	if err := s.policy.Approver.Approve(ctx, request); err != nil {
		if _, ok := policy.AsDenial(err); ok {
			recordDenial(ctx, policy.CheckApproval, s.agent)
			return nil, err
		}
		// The approver's own failure is kept as the cause: a timeout is not a refusal, and a caller
		// that wants to retry has to be able to tell them apart.
		telemetry.WarnErr(ctx, "host: the approver failed", err)
		recordDenial(ctx, policy.CheckApproval, s.agent)
		// The reason is rendered back to the model, so it is a fixed sentence: an approver that
		// failed on an HTTP call, a policy engine or a file read can put paths, endpoints or a
		// token fragment in its error text, and none of that belongs in a prompt. The cause is
		// wrapped, so a caller can still tell a failure from a refusal and can read the detail.
		return nil, &policy.Denial{
			Check:  policy.CheckApproval,
			Reason: "the approval service failed; the command was not run",
			Cause:  err,
		}
	}
	return s.next.Start(ctx, spec)
}

// refusal builds the denial for a command nothing approved, and counts it.
//
// The reason is rendered back to the model, so it says what was refused without being a log line.
func refusal(ctx context.Context, agentName, reason string) error {
	recordDenial(ctx, policy.CheckApproval, agentName)
	return &policy.Denial{Check: policy.CheckApproval, Reason: strings.TrimSpace(reason)}
}

// asDenial reports whether err is a policy refusal, which is what the host's own rules return.
func asDenial(err error) (*policy.Denial, bool) { return policy.AsDenial(err) }

// starterFor wraps the backend's starter in the host's approval rule, when there is one.
func starterFor(runner sandbox.Runner, approval *Approval, agentName, sessionID string) sandbox.SessionStarter {
	starter := sandbox.SessionStarter(runner)
	if approval == nil {
		return starter
	}
	return approvalStarter{next: starter, policy: *approval, agent: agentName, session: sessionID}
}
