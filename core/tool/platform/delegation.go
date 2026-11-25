package platform

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

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
	// Answer continues a child that paused for input.
	Answer(ctx context.Context, session, answer string) (DelegationOutcome, error)
	// Cancel stops a child. A child that already finished reports nil.
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
