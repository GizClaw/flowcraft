package platform

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// The decisions a parent can give a delegated child that asked for approval.
const (
	// DecisionApprove lets the child run the command it asked about.
	DecisionApprove = "approve"
	// DecisionDeny refuses it. A denial is final: the child cannot escalate it afterwards.
	DecisionDeny = "deny"
	// DecisionEscalate hands the question to the deployment's approver, which is where a parent
	// that does not want to judge its own child's command sends it.
	DecisionEscalate = "escalate"
)

// DelegationAnswer is what a caller sends back to a paused child.
//
// A child pauses for two reasons, and they are answered differently: an approval question takes a
// [DecisionApprove], [DecisionDeny] or [DecisionEscalate], while any other question takes the text
// the child continues from. The two are one value because the tool that sends them is one tool, and
// a model should not have to know which kind of pause it is answering before it can call it.
type DelegationAnswer struct {
	// Decision is one of the Decision constants, empty for a free-text answer.
	Decision string
	// Text is the answer to a question that is not an approval, or the reason a parent gives
	// beside its decision.
	Text string
}

// Delegation is how a tool hands work to another agent.
//
// It is a capability rather than a host reference for the same reason every other slot is: a tool may
// not reach the host, and the host is the only thing that can open a session, run a turn and know when
// the child is done. What crosses the seam is data — a request and what came back — so neither side
// depends on the other's types.
//
// The mode and the target arrive as strings, which is what the model typed; deciding whether this
// session may delegate there, that way, at this depth is the host's, because only it holds the grant
// and the lineage.
type Delegation interface {
	// Delegate runs one child agent and returns what it produced.
	//
	// A refusal — the target is not in the policy, the mode is not allowed, the chain is too deep — is
	// an error, not an Outcome: a caller that cannot tell "it refused" from "the child failed" cannot
	// report either.
	Delegate(ctx context.Context, req DelegationRequest) (DelegationOutcome, error)
	// Status reports where a child stands, by the session Delegate returned.
	Status(ctx context.Context, session string) (DelegationOutcome, error)
	// Answer continues a child that paused for input: an approval decision, or the text the child
	// asked for.
	Answer(ctx context.Context, session string, answer DelegationAnswer) (DelegationOutcome, error)
	// Cancel stops a child, including one paused for an answer. A child that is already gone
	// reports nil: the caller's intent is satisfied either way.
	Cancel(ctx context.Context, session, reason string) error
}

// DelegationRequest is one delegated run, as the model asked for it.
type DelegationRequest struct {
	// Mode is "sync" or "async". The protocol spells them; the host decides whether the grant allows
	// the one that was asked for.
	Mode string
	// Target is the deployed agent to run.
	Target string
	// Input is the message the child starts from.
	Input string
}

// DelegationOutcome is what a delegated run produced.
type DelegationOutcome struct {
	// Status is the child's terminal state, empty when it is still running (an async hand-off).
	Status string
	// Session is the child session this delegation created, so a caller can address it later.
	Session string
	// Agent is the deployment the child ran: the target the caller named.
	Agent string
	// Content is the child's answer, as the events folded it.
	Content message.Content
	// Question is what the child asked when it paused for input.
	Question *message.Content
	// Chain is the root delegation this run belongs to, and how many edges above it.
	Chain string
	Depth int
}

// DelegationSlot is the capability a tool declares in [tool.Tool.Needs] to delegate.
var DelegationSlot = tool.NewSlot[Delegation]("delegation.sessions")
