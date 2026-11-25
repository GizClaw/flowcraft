package host

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
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

// approvalStarter is the decorator the session's process capability carries when a policy or an
// escalation declaration exists.
//
// Two rules, in this order and never the other way round: the command has to be allowed to run at
// all, and only a command the boundary then refuses is offered for a widening. Asking an operator to
// widen a boundary for a command the other operator already refused would be two answers to one
// question, and the second one would override the first.
type approvalStarter struct {
	next sandbox.SessionStarter
	// policy is the host's approval rule; nil when this process configured none.
	policy  *Approval
	agent   string
	session string
	// widener is the session's escalation state; nil when its deployment declares none.
	widener *widener
	// factory builds a temporary cell from a widened grant, and audit keeps the record a widening
	// that ran leaves behind.
	factory   BackendFactory
	escalator Escalator
	audit     func(context.Context, escalationRecord)
	// route carries this child's questions to the parent that opened it; nil for a session a
	// caller opened, which asks the deployment's approver inline as it always did.
	route *approvalRoute
}

// Ask reports the question that must be answered before this command runs, without running it.
//
// It is the pre-flight the tool node calls: a command the whitelist covers has no question, one the
// parent already answered has none either, and everything else becomes a question the child's turn
// suspends on. Only a delegated child has a route; a root session answers inline in [admit], which
// is why this returns no question for it.
func (s approvalStarter) Ask(_ context.Context, spec sandbox.SessionSpec) (*message.Content, error) {
	if s.policy == nil || s.route == nil || s.policy.Whitelist.Covers(spec.Argv) {
		return nil, nil
	}
	return s.route.ask(spec), nil
}

// Start runs the command under both of the host's rules.
func (s approvalStarter) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	running, err := s.admit(ctx, spec)
	if !errors.Is(err, sandbox.ErrConfineDenied) || s.widener == nil {
		return running, err
	}
	return s.escalate(ctx, spec, err)
}

// admit applies the approval rule and starts the command in the session's own cell.
func (s approvalStarter) admit(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if s.policy == nil || s.policy.Whitelist.Covers(spec.Argv) {
		return s.next.Start(ctx, spec)
	}
	request := ApprovalRequest{
		Agent:     s.agent,
		SessionID: s.session,
		Argv:      append([]string(nil), spec.Argv...),
		WorkDir:   spec.Opts.WorkDir,
		TTY:       spec.TTY,
	}
	// A delegated child's answer arrives from its parent, one command at a time. A denial is an
	// answer like any other: the command does not run, and the model is told which rule refused it.
	if s.route != nil {
		if answer, ok := s.route.lookup(commandDigest(spec.Argv, spec.Opts.WorkDir)); ok {
			if answer.approve {
				return s.next.Start(ctx, spec)
			}
			recordDenial(ctx, policy.CheckApproval, s.agent)
			reason := "the parent refused this command"
			if answer.note != "" {
				reason += ": " + answer.note
			}
			return nil, &policy.Denial{Check: policy.CheckApproval, Reason: reason}
		}
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

// escalate asks for a widening of this one command and, when it is granted, runs the command once
// in a temporary cell built from the widened grant.
//
// The retry happens once. A second refusal inside the widened cell is the answer, because a model
// that could re-ask would turn the confine into a prompt it can summon, and an operator answering
// the same question in a loop is not a decision.
//
// Every outcome is counted, and only an attempt that actually ran is written to the ledger: a
// refusal, a spent budget or a missing escalator changed nothing, and a record of "nothing
// happened" per attempt is a log a model can inflate by walking into its boundary.
func (s approvalStarter) escalate(ctx context.Context, spec sandbox.SessionSpec, refused error) (sandbox.Session, error) {
	if s.escalator == nil {
		// Nothing to ask: the command is outside the session's authority and no operator has
		// offered to widen it, so the refusal is the answer.
		return nil, refused
	}
	from := s.widener.bind
	request := s.widener.request(s.agent, s.session, spec)

	asked := time.Now()
	answer, err := s.escalator.ApproveWidening(ctx, request)
	if err != nil {
		recordEscalation(ctx, s.agent, escalationDenied)
		if _, ok := policy.AsDenial(err); !ok {
			telemetry.WarnErr(ctx, "host: the escalator failed", err)
		}
		// The original refusal is what the model gets: the command did not run, and what it needs
		// to know is that. The operator's reason belongs to the operator.
		return nil, refused
	}
	widened, err := from.Widen(answer)
	if err != nil {
		// An answer that is not a widening of what the session had is refused, not trusted: an
		// approval widens one boundary, it does not replace the session's authority.
		recordEscalation(ctx, s.agent, escalationUnauthorized)
		telemetry.WarnErr(ctx, "host: the escalator answered with something that is not a widening", err)
		return nil, refused
	}
	attemptCtx, attempt, cancel := s.widener.attempt(ctx, spec)
	running, err := temporaryCell(attemptCtx, cancel, s.factory, widened, attempt)
	switch {
	case err == nil:
		s.recordWidening(ctx, spec, widened, time.Since(asked), escalationApproved)
		return running, nil
	case errors.Is(err, sandbox.ErrConfineDenied):
		// The widened boundary refused the command too. One retry is the whole offer, so this is
		// the answer and it is returned as it came — never as a second question, or a model could
		// summon prompts by walking into whatever boundary it is given.
		s.recordWidening(ctx, spec, widened, time.Since(asked), escalationApproved)
		return nil, err
	default:
		// The temporary cell never started a command: the widening was granted but nothing ran,
		// so there is nothing to write down.
		recordEscalation(ctx, s.agent, escalationUnavailable)
		telemetry.WarnErr(ctx, "host: the widened cell did not start the command", err)
		return nil, refused
	}
}

// recordWidening counts the widening and writes what it left behind.
//
// It runs for every widening that reached a temporary cell, whatever the command then did: the
// decision is the fact, and a ledger that only recorded the successful commands would lose exactly
// the widenings worth reviewing.
func (s approvalStarter) recordWidening(ctx context.Context, spec sandbox.SessionSpec, widened policy.Grant, took time.Duration, outcome string) {
	recordEscalation(ctx, s.agent, outcome)
	escalated(ctx, s.widener.bind.Digest(), widened.Digest())
	record := escalationRecord{
		Check:       escalationKind,
		CommandHash: commandHash(spec.Argv, spec.Opts.WorkDir),
		FromBind:    s.widener.bind.Digest(),
		ToBind:      widened.Digest(),
		Approver:    describe(s.escalator),
		Decision:    outcome,
		DurationMS:  took.Milliseconds(),
	}
	s.audit(ctx, record)
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

// starterFor wraps the backend's starter in the host's rules, when there are any.
//
// A process with no approval policy and a session whose deployment declares no escalation keeps the
// backend's own starter, undecorated: both rules are opt-in, and a decorator that forwarded six
// methods would be the cost of pretending otherwise.
func starterFor(runner sandbox.Runner, s approvalStarter) sandbox.SessionStarter {
	s.next = runner
	if s.policy == nil && s.widener == nil {
		return runner
	}
	return s
}
