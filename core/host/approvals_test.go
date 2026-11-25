package host

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// askingChild opens one session as if a delegation had opened it: the same machinery, with the
// lineage a child carries, so its questions have somewhere to go.
func askingChild(t *testing.T, approval Approval, chain identity.Chain) (*Agents, *Session, sandbox.SessionStarter, *gate) {
	t.Helper()
	log := &journal{}
	backend := &gate{log: log}
	res, err := Open(t.Context(), deploy.ProcessPlan{Backends: []deploy.PlannedBackend{
		boundBackend("local", &boundary{log: log, filesystemBounds: true}),
	}}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	res.backends["local"] = func(sandbox.Confine) (sandbox.Runner, error) { return backend, nil }

	agents := NewAgents(res, WithApproval(approval))
	dep, _ := sessionDeployment(t, "child", permissiveGrant("local"), log, &boundary{log: log})
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, err := agents.Open(t.Context(), "p1/child-1", "child", permissiveGrant("local"), withChain(chain))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	starter, ok := platform.ProcessSlot.Get(session.Binding().Caps)
	if !ok || starter == nil {
		t.Fatal("the session has no process capability")
	}
	return agents, session, starter, backend
}

func lineage(parent string) identity.Chain {
	return identity.Chain{ID: "chain-1", Depth: 1, Lineage: []string{parent}}
}

// askerOf is the pre-flight the tool node sees, asserted the way the node asserts it.
func askerOf(t *testing.T, starter sandbox.SessionStarter) interface {
	Ask(context.Context, sandbox.SessionSpec) (*message.Content, error)
} {
	t.Helper()
	asker, ok := starter.(interface {
		Ask(context.Context, sandbox.SessionSpec) (*message.Content, error)
	})
	if !ok {
		t.Fatal("the starter does not expose a pre-flight")
	}
	return asker
}

// TestAQuestionNamesTheCommandAndWhereToAnswer is what the parent's model reads: the command it is
// judging, and the address its answer has to name.
func TestAQuestionNamesTheCommandAndWhereToAnswer(t *testing.T) {
	_, _, starter, _ := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	question, err := askerOf(t, starter).Ask(t.Context(), spec("git", "push", "--force"))
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if question == nil {
		t.Fatal("a command that is not whitelisted was not asked about")
	}
	text := question.Text()
	for _, want := range []string{"p1/child-1", "git push --force", pathtest.Abs("srv"), "delegation_answer"} {
		if !strings.Contains(text, want) {
			t.Fatalf("question = %q, want it to name %q", text, want)
		}
	}
}

// TestAWhitelistedCommandIsNotAsked keeps the rule's first half: a command the deployment already
// decided about never reaches the parent.
func TestAWhitelistedCommandIsNotAsked(t *testing.T) {
	_, _, starter, _ := askingChild(t, Approval{Whitelist: Whitelist{{"git", "status"}}}, lineage("p1"))
	question, err := askerOf(t, starter).Ask(t.Context(), spec("git", "status"))
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if question != nil {
		t.Fatalf("question = %q, want none for a whitelisted command", question.Text())
	}
}

// TestARootSessionAsksNobody is the other branch: no lineage means no parent to ask, so the pre-flight
// stays silent and the deployment's approver answers inline as it always did.
func TestARootSessionAsksNobody(t *testing.T) {
	_, session, starter, _ := askingChild(t, Approval{Approver: &reviewer{}}, identity.Chain{})
	if session.approvals != nil {
		t.Fatal("a root session has a route to a parent it does not have")
	}
	if _, ok := starter.(interface {
		Ask(context.Context, sandbox.SessionSpec) (*message.Content, error)
	}); !ok {
		t.Fatal("the starter lost its pre-flight")
	}
}

// TestAnAnsweredQuestionLetsTheCommandRun is the whole point: approve records the decision, the
// child's next pre-flight has no question, and the command reaches the backend.
func TestAnAnsweredQuestionLetsTheCommandRun(t *testing.T) {
	_, session, starter, backend := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	command := spec("git", "push")
	if _, err := askerOf(t, starter).Ask(t.Context(), command); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), platform.DecisionApprove, "go ahead"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	question, err := askerOf(t, starter).Ask(t.Context(), command)
	if err != nil || question != nil {
		t.Fatalf("Ask after the answer = %v, %v; want no question", question, err)
	}
	_ = start(t, starter, command)
	if len(backend.started) != 1 {
		t.Fatal("the approved command never reached the backend")
	}
}

// TestARefusedCommandDoesNotRun is the other verdict, and it names the rule that refused.
func TestARefusedCommandDoesNotRun(t *testing.T) {
	_, session, starter, backend := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	command := spec("git", "push")
	if _, err := session.approvals.decide(t.Context(), platform.DecisionDeny, "not to main"); !errdefs.IsValidation(err) {
		t.Fatalf("decide without a question = %v, want Validation", err)
	}
	if _, err := askerOf(t, starter).Ask(t.Context(), command); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), platform.DecisionDeny, "not to main"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	err := start(t, starter, command)
	if _, denied := policy.AsDenial(err); !denied {
		t.Fatalf("Start = %v, want a policy denial", err)
	}
	if len(backend.started) != 0 {
		t.Fatal("a refused command reached the backend")
	}
	if strings.Contains(err.Error(), "approver") {
		t.Fatalf("err = %v, want the parent's refusal, not the deployment's", err)
	}
}

// TestAnAnswerIsBoundToItsCommand keeps a decision where it was made: one workdir's approval is not
// another's, and the digest says so.
func TestAnAnswerIsBoundToItsCommand(t *testing.T) {
	_, session, starter, _ := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	approved := spec("git", "push")
	if _, err := askerOf(t, starter).Ask(t.Context(), approved); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), platform.DecisionApprove, ""); err != nil {
		t.Fatalf("decide: %v", err)
	}
	elsewhere := approved
	elsewhere.Opts.WorkDir = pathtest.Abs("other")
	question, err := askerOf(t, starter).Ask(t.Context(), elsewhere)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if question == nil {
		t.Fatal("an approval for one directory covered another")
	}
}

// TestAnEscalationAsksTheDeployment is the parent's way out: it does not want to judge its own
// child's command, so the deployment's approver does.
func TestAnEscalationAsksTheDeployment(t *testing.T) {
	reviewer := &reviewer{}
	_, session, starter, backend := askingChild(t, Approval{Approver: reviewer}, lineage("p1"))
	command := spec("rm", "-rf", pathtest.Abs("srv"))
	if _, err := askerOf(t, starter).Ask(t.Context(), command); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), platform.DecisionEscalate, "you decide"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(reviewer.asked) != 1 {
		t.Fatalf("the deployment's approver was asked %d times, want 1", len(reviewer.asked))
	}
	if got := strings.Join(reviewer.asked[0].Argv, " "); got != "rm -rf "+pathtest.Abs("srv") {
		t.Fatalf("the approver saw %q", got)
	}
	if _, err := askerOf(t, starter).Ask(t.Context(), command); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	_ = start(t, starter, command)
	if len(backend.started) != 1 {
		t.Fatal("the escalated command never ran")
	}
}

// TestAnEscalationWithoutAReviewerIsRefused: there is nobody above a parent that has nothing above
// it, and pretending otherwise would be a question with no answer.
func TestAnEscalationWithoutAReviewerIsRefused(t *testing.T) {
	_, session, starter, _ := askingChild(t, Approval{}, lineage("p1"))
	if _, err := askerOf(t, starter).Ask(t.Context(), spec("ls")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), platform.DecisionEscalate, ""); !errdefs.IsNotAvailable(err) {
		t.Fatalf("decide = %v, want NotAvailable", err)
	}
}

// TestAnEscalationKeepsTheReviewersFailureApart: a failure is not a refusal, and the cause survives.
func TestAnEscalationKeepsTheReviewersFailureApart(t *testing.T) {
	cause := errors.New("the approval service is down")
	reviewer := &reviewer{err: cause}
	_, session, starter, _ := askingChild(t, Approval{Approver: reviewer}, lineage("p1"))
	if _, err := askerOf(t, starter).Ask(t.Context(), spec("ls")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), platform.DecisionEscalate, ""); !errors.Is(err, cause) {
		t.Fatalf("decide = %v, want the approver's failure", err)
	}
}

// TestACancelDoesNotAnswer keeps the two vocabularies apart: a decision is one of three words, and
// anything else is a validation error rather than a silent denial.
func TestACancelDoesNotAnswer(t *testing.T) {
	_, session, starter, _ := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	if _, err := askerOf(t, starter).Ask(t.Context(), spec("ls")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), "cancel", ""); !errdefs.IsValidation(err) {
		t.Fatalf("decide = %v, want Validation", err)
	}
}

// TestAStaleAnswerIsNotUsed keeps a decision from outliving the work it was made for.
func TestAStaleAnswerIsNotUsed(t *testing.T) {
	route := newApprovalRoute("c", "child", "p1", nil)
	question := route.ask(spec("ls"))
	if question == nil {
		t.Fatal("no question")
	}
	route.mu.Lock()
	route.question.asked = time.Now().Add(-2 * approvalTTL)
	route.mu.Unlock()
	if _, err := route.decide(t.Context(), platform.DecisionApprove, ""); !errdefs.IsValidation(err) {
		t.Fatalf("decide = %v, want Validation for an expired question", err)
	}

	digest := specDigest(spec("ls"))
	route.decided[digest] = approvalDecision{approve: true, at: time.Now().Add(-2 * approvalTTL)}
	if _, ok := route.lookup(digest); ok {
		t.Fatal("an expired decision was still in force")
	}
}

// TestTheAnsweredTextTellsTheChildWhatToDo is the message the resumed child's model reads: a granted
// command is retried, a refusal is final.
func TestTheAnsweredTextTellsTheChildWhatToDo(t *testing.T) {
	_, session, starter, _ := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	command := spec("git", "push")
	if _, err := askerOf(t, starter).Ask(t.Context(), command); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	granted, err := session.answer(t.Context(), platform.DelegationAnswer{Decision: platform.DecisionApprove})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !strings.Contains(granted, "approved") || !strings.Contains(granted, "git push") {
		t.Fatalf("answer = %q, want the command named and the decision stated", granted)
	}

	if _, err := askerOf(t, starter).Ask(t.Context(), spec("git", "tag", "v1")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	refused, err := session.answer(t.Context(), platform.DelegationAnswer{Decision: platform.DecisionDeny, Text: "no tags"})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !strings.Contains(refused, "final") || !strings.Contains(refused, "do not retry") {
		t.Fatalf("answer = %q, want a refusal that cannot be escalated", refused)
	}
}

// TestAFreeTextAnswerIsPassedThrough keeps delegation_answer usable for a question that is not an
// approval: the child continues from what the parent typed.
func TestAFreeTextAnswerIsPassedThrough(t *testing.T) {
	_, session, _, _ := askingChild(t, Approval{}, lineage("p1"))
	text, err := session.answer(t.Context(), platform.DelegationAnswer{Text: "seat 14A"})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if text != "seat 14A" {
		t.Fatalf("answer = %q", text)
	}
	if _, err := session.answer(t.Context(), platform.DelegationAnswer{}); !errdefs.IsValidation(err) {
		t.Fatalf("empty answer = %v, want Validation", err)
	}
}

// TestTextCannotAnswerAnApproval keeps the two kinds of pause apart: a sentence does not decide
// whether a command runs, so the parent is told which vocabulary the question takes.
func TestTextCannotAnswerAnApproval(t *testing.T) {
	_, session, starter, _ := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	if _, err := askerOf(t, starter).Ask(t.Context(), spec("git", "push")); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.answer(t.Context(), platform.DelegationAnswer{Text: "sure"}); !errdefs.IsValidation(err) {
		t.Fatalf("answer = %v, want Validation for text on an approval", err)
	}
}

// TestARoutedChildNeverFallsBackToTheDeployment is the authority the feature exists to keep: a child
// whose command was not put to its parent is refused, not handed to a person who never delegated it.
func TestARoutedChildNeverFallsBackToTheDeployment(t *testing.T) {
	reviewer := &reviewer{}
	_, _, starter, backend := askingChild(t, Approval{Approver: reviewer}, lineage("p1"))
	err := start(t, starter, spec("git", "push"))
	if _, denied := policy.AsDenial(err); !denied {
		t.Fatalf("Start = %v, want a policy denial for a command that was never asked about", err)
	}
	if len(reviewer.asked) != 0 {
		t.Fatal("the deployment's approver was asked about a delegated child's command")
	}
	if len(backend.started) != 0 {
		t.Fatal("an unasked command reached the backend")
	}
}

// TestASecondAnswerCannotOverwriteTheFirst keeps a denial final under a batch: two answers in one
// turn both see the same open question, and the loser must be refused rather than silently winning
// the write.
func TestASecondAnswerCannotOverwriteTheFirst(t *testing.T) {
	_, session, starter, backend := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	command := spec("git", "push")
	if _, err := askerOf(t, starter).Ask(t.Context(), command); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), platform.DecisionDeny, "no"); err != nil {
		t.Fatalf("first decide: %v", err)
	}
	if _, err := session.approvals.decide(t.Context(), platform.DecisionApprove, "yes"); err == nil {
		t.Fatal("a second answer overwrote the first")
	} else if !errdefs.IsConflict(err) && !errdefs.IsValidation(err) {
		t.Fatalf("second decide = %v, want Conflict or Validation", err)
	}
	if decision, ok := session.approvals.lookup(specDigest(command)); !ok || decision.approve {
		t.Fatalf("stored decision = %+v, %v; want the denial", decision, ok)
	}
	if len(backend.started) != 0 {
		t.Fatal("the command ran")
	}
}

// TestAPausedChildCanBeCancelled is the lifecycle rule the docs state: a child with no turn in flight
// has nothing to interrupt, so a cancel is what closes it.
func TestAPausedChildCanBeCancelled(t *testing.T) {
	agents, _, _, _ := askingChild(t, Approval{Approver: &reviewer{}}, lineage("p1"))
	parent, ok := agents.Session("p1/child-1")
	if !ok {
		t.Fatal("the child is not open")
	}
	delegator := &delegator{agents: agents, parent: "p1", agent: "parent", grant: parent.grant, chain: parent.chain}
	if err := delegator.Cancel(t.Context(), "p1/child-1", "the parent gave up"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, open := agents.Session("p1/child-1"); open {
		t.Fatal("a cancelled paused child is still open")
	}
}
