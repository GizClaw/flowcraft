// Package inject is the only writer of run identity.
//
// Three call sites may decide who a run is, and each knows exactly one fact:
//
//   - the host, when it opens a session (it is the only layer that knows the
//     grant the caller stated, and therefore the bind);
//   - the agent, when it starts a turn (the only layer that knows the task and the
//     ordinal);
//   - delegation, when it derives a child run (the only layer that knows the
//     lineage).
//
// Every other layer reads through [identity.Lookup]. The rule is enforced rather
// than documented: TestOnlyHostAndAgentImportInject pins the importers, so a new
// package cannot start naming identities by accident.
//
// # Why these return an error instead of merging
//
// A second Session or Turn at the same level is a programming error, not a
// refinement. Merging field by field is what made the previous carrier rot: a layer
// that "helpfully" filled in a missing field left two spans in one trace
// disagreeing about which session they belonged to, and nothing failed. An error
// makes the mistake land where it was made. It is an error rather than a panic
// because this is a library: a host that retries an open must not lose the process
// to it.
package inject

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/identity"
)

// Session attaches the session facts. It requires an agent and a session id — the
// two fields every consumer may rely on — and refuses a context that already has a
// session.
func Session(ctx context.Context, session identity.Session) (context.Context, error) {
	if existing, ok := identity.Lookup(ctx); ok && existing.SessionID != "" {
		return ctx, errors.New("identity: a session is already attached; one trace carries one identity")
	}
	if session.Agent == "" {
		return ctx, errors.New("identity: agent is required")
	}
	if session.SessionID == "" {
		return ctx, errors.New("identity: session id is required")
	}
	run, _ := identity.Lookup(ctx)
	run.Tenant = session.Tenant
	run.Agent = session.Agent
	run.Authority = session.Authority
	run.SessionID = session.SessionID
	run.Bind = session.Bind
	return identity.Attach(ctx, run), nil
}

// Turn attaches the turn facts. It requires an attached session: a turn without a
// session has nowhere to belong, and guessing which session it meant is how spans
// get attributed to the wrong conversation.
func Turn(ctx context.Context, turn identity.Turn) (context.Context, error) {
	if turn.Ordinal < 1 {
		return ctx, errors.New("identity: the turn ordinal is 1-based")
	}
	run, ok := identity.Lookup(ctx)
	if !ok || run.SessionID == "" {
		return ctx, errors.New("identity: a turn requires an attached session")
	}
	if run.Ordinal != 0 {
		return ctx, errors.New("identity: this run already has a turn; derive a new context per run")
	}
	run.TaskID = turn.TaskID
	run.Ordinal = turn.Ordinal
	return identity.Attach(ctx, run), nil
}

// Chain attaches the delegation lineage. It requires an attached session and an id,
// and refuses a context that is already a chain member: a run descends from exactly
// one caller.
func Chain(ctx context.Context, chain identity.Chain) (context.Context, error) {
	if chain.ID == "" {
		return ctx, errors.New("identity: chain id is required")
	}
	if chain.Depth < 0 {
		return ctx, errors.New("identity: chain depth must not be negative")
	}
	run, ok := identity.Lookup(ctx)
	if !ok || run.SessionID == "" {
		return ctx, errors.New("identity: a chain requires an attached session")
	}
	if run.ChainID != "" {
		return ctx, errors.New("identity: this run is already in a chain")
	}
	run.ChainID = chain.ID
	run.Depth = chain.Depth
	return identity.Attach(ctx, run), nil
}
