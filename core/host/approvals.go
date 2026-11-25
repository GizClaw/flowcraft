package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// approvalTTL bounds how long an answer stays usable.
//
// An answer is a decision about one command, and a decision that outlives the work it was made for
// is authority nobody granted: a child that keeps a granted command in its transcript for an hour
// must ask again, not replay. The window is generous because a parent may be a person answering a
// different conversation, and a refusal is not "ask again in a second".
const approvalTTL = 15 * time.Minute

// approvalRouteFor builds the route a session's command questions travel, or nil when the session
// has no parent: a root session asks the deployment's approver inline, on the turn's goroutine.
func (a *Agents) approvalRouteFor(sessionID, agentName string, chain identity.Chain) *approvalRoute {
	if len(chain.Lineage) == 0 {
		return nil
	}
	parent := chain.Lineage[len(chain.Lineage)-1]
	var human func(context.Context, ApprovalRequest) error
	if a.approval != nil && a.approval.Approver != nil {
		human = a.approval.Approver.Approve
	}
	return newApprovalRoute(sessionID, agentName, parent, human)
}

// approvalQuestion is what a delegated child asked and has not been answered.
type approvalQuestion struct {
	request ApprovalRequest
	// digest names the command line and the directory it would run in: one question, one answer,
	// and an approval for `git status` is not an approval for `git push`.
	digest string
	text   message.Content
	asked  time.Time
}

// approvalDecision is the answer a child may act on.
type approvalDecision struct {
	approve bool
	// note is the parent's own words, kept for the transcript and the log; empty for "no reason
	// given".
	note string
	at   time.Time
}

// approvalRoute carries a delegated child's command questions to the parent that opened it.
//
// It is the "ask before you run" half of [Approval]: the process's rule still says *whether* a
// command needs an answer, and the route says *who* gives it. A session a caller opened has no
// route and keeps the old behaviour — the deployment's [Approver], called on the turn's goroutine.
// A delegated child has one, and its questions go to the parent's model first: the agent that asked
// for the work has the context to judge the command, and the process's approver is what a parent
// escalates to when it does not want to decide.
type approvalRoute struct {
	mu      sync.Mutex
	session string
	agent   string
	// parent is the session that opened this one: the delegation's caller.
	parent string
	// human is the deployment's approver, asked only when the parent escalates.
	human func(context.Context, ApprovalRequest) error

	question *approvalQuestion
	decided  map[string]approvalDecision
}

func newApprovalRoute(session, agent, parent string, human func(context.Context, ApprovalRequest) error,
) *approvalRoute {
	return &approvalRoute{
		session: session,
		agent:   agent,
		parent:  parent,
		human:   human,
		decided: map[string]approvalDecision{},
	}
}

// ask records the question a command raised and returns the text the turn suspends on.
//
// It is the tool node's pre-flight, so it must not run anything: the child's turn stops here, the
// question reaches the parent, and the same call is offered again when the child resumes.
func (r *approvalRoute) ask(spec sandbox.SessionSpec) *message.Content {
	request := ApprovalRequest{
		Agent:     r.agent,
		SessionID: r.session,
		Argv:      append([]string(nil), spec.Argv...),
		WorkDir:   spec.Opts.WorkDir,
		TTY:       spec.TTY,
	}
	digest := commandDigest(spec.Argv, spec.Opts.WorkDir)

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, answered := r.decided[digest]; answered {
		return nil
	}
	if r.question != nil && r.question.digest == digest {
		text := r.question.text
		return &text
	}
	question := approvalQuestion{
		request: request,
		digest:  digest,
		text:    message.NewTextContent(approvalQuestionText(request)),
		asked:   time.Now(),
	}
	// A different command replaces the open question: the turn was suspended on the first one, so
	// reaching here with another means the child re-planned, and the parent is asked about what it
	// would actually run.
	r.question = &question
	text := question.text
	return &text
}

// decide records the parent's answer, resolving an escalation to the human first.
//
// It is called from the parent's turn, through delegation_answer, and returns the question that was
// answered so the caller can name the command in what it sends back.
func (r *approvalRoute) decide(ctx context.Context, decision, note string) (approvalQuestion, error) {
	r.mu.Lock()
	question := r.question
	if question == nil {
		r.mu.Unlock()
		return approvalQuestion{}, errdefs.Validationf(
			"host: %q has no question waiting for an answer", r.session)
	}
	if time.Since(question.asked) > approvalTTL {
		r.question = nil
		r.mu.Unlock()
		return approvalQuestion{}, errdefs.Validationf(
			"host: the question from %q expired; ask it again from the child", r.session)
	}
	r.mu.Unlock()

	answer := approvalDecision{note: note, at: time.Now()}
	switch decision {
	case platform.DecisionApprove:
		answer.approve = true
	case platform.DecisionDeny:
	case platform.DecisionEscalate:
		// The parent does not want to judge its own child's command: the deployment's approver
		// answers instead. Its refusal is the answer, so a human's "no" ends here too.
		if r.human == nil {
			return approvalQuestion{}, errdefs.NotAvailablef(
				"host: nothing to escalate to: this process installs no approver")
		}
		if err := r.human(ctx, question.request); err != nil {
			if _, ok := policy.AsDenial(err); !ok {
				return approvalQuestion{}, err
			}
			answer.note = strings.TrimPrefix(err.Error(), "policy denied (approval): ")
			answer.approve = false
		} else {
			answer.approve = true
		}
	default:
		return approvalQuestion{}, errdefs.Validationf(
			"host: %q is not a decision (approve, deny or escalate)", decision)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.decided[question.digest] = answer
	r.question = nil
	return *question, nil
}

// lookup reports the answer this child may act on, if it has one that has not expired.
func (r *approvalRoute) lookup(digest string) (approvalDecision, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	decision, ok := r.decided[digest]
	if !ok {
		return approvalDecision{}, false
	}
	if time.Since(decision.at) > approvalTTL {
		delete(r.decided, digest)
		return approvalDecision{}, false
	}
	return decision, true
}

// open reports whether the child is suspended on a question.
func (r *approvalRoute) open() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.question != nil
}

// commandDigest names a command and the directory it would run in.
//
// The separator and the workdir are part of the digest because a decision is about an invocation:
// approving `git push` in one checkout must not approve it in another, and two argv lists that
// merely concatenate to the same string are not the same command.
func commandDigest(argv []string, workdir string) string {
	sum := sha256.New()
	for _, word := range argv {
		sum.Write([]byte(word))
		sum.Write([]byte{0})
	}
	sum.Write([]byte{1})
	sum.Write([]byte(workdir))
	return hex.EncodeToString(sum.Sum(nil))
}

// recordDelegationDecision counts a child's approval questions by the answer a parent gave.
func recordDelegationDecision(ctx context.Context, agentName, decision string) {
	meter := telemetry.MeterWithSuffix("host")
	counter, err := meter.Int64Counter("host.delegation_approvals",
		metric.WithDescription("Command approvals a delegated child asked its parent for, by decision"))
	if err != nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("decision", decision)}
	if agentName != "" {
		attrs = append(attrs, attribute.String(identity.AttrAgentName, agentName))
	}
	counter.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// approvalQuestionText is what the child's turn asks and the parent reads.
//
// It names the command, the directory and the session to answer with, because the parent's answer
// arrives through a tool call that has to address the right child.
func approvalQuestionText(request ApprovalRequest) string {
	var body strings.Builder
	body.WriteString("[approval] a delegated agent wants to run a command\n")
	body.WriteString("agent: " + request.Agent + "\n")
	body.WriteString("session: " + request.SessionID + "\n")
	body.WriteString("command: " + strings.Join(request.Argv, " ") + "\n")
	if request.WorkDir != "" {
		body.WriteString("workdir: " + request.WorkDir + "\n")
	}
	if request.TTY {
		body.WriteString("terminal: yes\n")
	}
	body.WriteString("answer with delegation_answer(session: " + request.SessionID +
		", decision: approve|deny|escalate); escalate asks the deployment's approver.\n")
	return body.String()
}
