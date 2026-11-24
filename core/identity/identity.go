// Package identity is who a run is: the tenant, the agent, the session, the
// authority and bind the session was narrowed from, the task, the turn, and the
// delegation chain it belongs to.
//
// # A value with no dependencies
//
// The package imports nothing from core. Every layer below the assembly reads the
// identity to start a span — inferencestreams, sandbox commands, tool dispatches —
// and a carrier that pulled in the telemetry SDK or the policy vocabulary would be
// the package everyone has to link for seven strings. The attribute names are
// strings here for the same reason; telemetry renders them.
//
// # Read here, write only through inject
//
// [Lookup] is the read path. The write path is [Attach], which exists for
// core/identity/inject (the three injection points) and for tests; a package that
// calls it directly is a bug, and TestOnlyInjectWritesIdentity enforces that rather
// than leaving it to review.
//
// # One trace has one identity
//
// The three facts are attached, never merged field by field. A second injection at
// the same level is a programming error, not a refinement: the layer that knows the
// session is the only one that may name it, and a layer that "helpfully" filled in a
// missing field is how two spans in one trace stop agreeing about which session they
// belong to.
package identity

import (
	"context"
	"strconv"
)

// Attribute keys. They are the canonical spelling; a dashboard, an alert and a log
// query all filter on these strings, so renaming one is a breaking change for every
// consumer.
const (
	// AttrTenantID identifies the tenant a run works for. Empty in a
	// single-tenant deployment.
	AttrTenantID = "tenant.id"
	// AttrAgentName names the deployed agent: the logical identity that outlives
	// one session.
	AttrAgentName = "agent.name"
	// AttrAuthorityID is the digest of the agent's grant: what this run may do at
	// its widest.
	AttrAuthorityID = "agent.authority.id"
	// AttrSessionID identifies the conversation. It is the checkpoint key and the
	// one identity a resumed run keeps.
	AttrSessionID = "session.id"
	// AttrBindID is the digest of the session's narrowed grant: the permission
	// actually in force. It answers "can this resume" as precisely as the
	// checkpoint fingerprint does.
	AttrBindID = "session.bind.id"
	// AttrTaskID identifies the task a step belongs to.
	AttrTaskID = "task.id"
	// AttrTurn is the 1-based ordinal of the run within the session.
	AttrTurn = "turn"
	// AttrChainID identifies the root delegation a run descends from. Empty for a
	// run that was never delegated.
	AttrChainID = "chain.id"
	// AttrChainDepth counts the delegation edges above this run; a root caller is
	// 0 and is not rendered.
	AttrChainDepth = "chain.depth"
)

// Session is what a session is: fixed when it opens, and unchanged for its life.
type Session struct {
	Tenant    string
	Agent     string
	SessionID string
	// Authority is the digest of the grant the agent was deployed with.
	Authority string
	// Bind is the digest of the grant this session actually runs under.
	Bind string
}

// Turn is one run inside a session.
type Turn struct {
	TaskID string
	// Ordinal is the 1-based position of the run within the session. It is
	// correlation, not identity: it is not persisted and restarts after eviction.
	Ordinal int
}

// Chain is the delegation lineage a run descends from.
type Chain struct {
	ID string
	// Depth is the number of delegation edges above this run.
	Depth int
}

// Run is the whole of what a span needs to say who it belongs to.
//
// It is flat rather than three embedded values: promotion would make Run.ID mean
// "the chain id", and a struct whose field names have to be looked up twice is not
// a value three layers read on every span.
type Run struct {
	Tenant    string
	Agent     string
	Authority string
	SessionID string
	Bind      string

	TaskID  string
	Ordinal int

	ChainID string
	Depth   int
}

// IsZero reports whether the value carries nothing.
func (r Run) IsZero() bool { return r == Run{} }

// Pairs renders the identity as ordered key/value strings, omitting what is unset.
//
// A renderer maps these onto whatever its backend calls an attribute; nothing here
// knows about one. The order is the contract: every producer that renders identity
// gets the same keys in the same order, so a diff between two spans is a diff of
// values rather than of attribute ordering.
func (r Run) Pairs() [][2]string {
	pairs := make([][2]string, 0, 9)
	add := func(key, value string) {
		if value != "" {
			pairs = append(pairs, [2]string{key, value})
		}
	}
	add(AttrTenantID, r.Tenant)
	add(AttrAgentName, r.Agent)
	add(AttrAuthorityID, r.Authority)
	add(AttrSessionID, r.SessionID)
	add(AttrBindID, r.Bind)
	add(AttrTaskID, r.TaskID)
	if r.Ordinal > 0 {
		pairs = append(pairs, [2]string{AttrTurn, strconv.Itoa(r.Ordinal)})
	}
	add(AttrChainID, r.ChainID)
	if r.Depth > 0 {
		pairs = append(pairs, [2]string{AttrChainDepth, strconv.Itoa(r.Depth)})
	}
	return pairs
}

type runKey struct{}

// Lookup returns the identity carried by ctx, if any.
func Lookup(ctx context.Context) (Run, bool) {
	run, ok := ctx.Value(runKey{}).(Run)
	return run, ok
}

// Detach returns ctx without a run identity.
//
// It exists for one case: a run started from inside another run — a delegated child — must
// attach its own identity, and the injection points refuse to overwrite one that is already
// there. Stripping the outer identity is the caller saying "this context is now mine", which is
// exactly what a nested run needs and what nothing else may do.
func Detach(ctx context.Context) context.Context {
	return context.WithValue(ctx, runKey{}, detachMarker{})
}

// detachMarker is what a detached context carries: a value of another type, so [Lookup]'s type
// assertion fails and no producer can mistake it for an identity.
type detachMarker struct{}

// Attach returns ctx carrying run.
//
// It is the only way to put identity on a context, and it is exported for
// core/identity/inject and for tests — not for producers. A layer that calls it
// directly is deciding who a run is, which only the three injection points may do.
func Attach(ctx context.Context, run Run) context.Context {
	return context.WithValue(ctx, runKey{}, run)
}
