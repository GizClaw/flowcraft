package sandbox

import (
	"context"
)

// Runner is a sandbox: it starts interactive process sessions under the
// sandbox's policy and manages their lifecycle. The one-shot form is a
// derived view over Start — approval.Exec drains a session and truncates its
// output, so a backend has no second code path to keep in step. The one
// documented override is a decorator that needs the completed result and
// implements the optional [Executor] capability; it carries the whole one-shot
// contract from then on. Implementations MUST honour ExecOptions.Timeout and
// reject any policy they cannot enforce with an errdefs.NotAvailable error
// rather than silently downgrading the request.
//
// Lifecycle is part of the contract, not a convention: Close releases
// every resource the runner owns — active sessions, forked daemons,
// sockets, backend state. Decorators MUST forward Close to their inner
// runner so wrapping never hides the backend's cleanup, and they MUST
// forward the optional capabilities — [JournalProvider.OpenJournal] and
// [SessionAttacher.Attach] — the same way: a decorator that swallows one
// while [Capabilities] keeps advertising it makes the declaration a lie.
// Embedding sandbox.Runner does not promote those methods; approval.Forwarder is
// the base that forwards them for you. It deliberately does not promote
// [Executor] either: a decorator that re-runs the command must be the outermost
// value, never wrapped by a plain Forwarder that would turn the override off
// without an error.
//
// Capabilities is mandatory: it is the explicit, wire-safe declaration
// of what this backend can do. Callers and tools never discover
// features through interface assertions — unsupported operations on a
// returned [Session] fail with errdefs.NotAvailable, and Capabilities
// says up front why.
type Runner interface {
	// Close releases every resource owned by the runner. It must be
	// safe to call more than once and when nothing was ever started;
	// implementations should terminate active sessions rather than
	// silently abandoning them.
	Close() error
	Capabilities() Capabilities
	Start(ctx context.Context, spec SessionSpec) (Session, error)
	List(ctx context.Context) ([]SessionInfo, error)
	Terminate(ctx context.Context, id string) error
}

// SessionStarter is the part of a runner a tool may use: it starts a session and
// nothing else.
//
// The split exists because managing sessions is a different authority from using
// one. A tool that may add a command channel needs Start; only the boundary's
// owner may enumerate or terminate what is running. Handing a tool a Runner would
// quietly hand it both.
type SessionStarter interface {
	Start(ctx context.Context, spec SessionSpec) (Session, error)
}

// Capabilities is the explicit declaration of a Runner's surface: what
// policy it can enforce and which session features it can provide. It
// travels unchanged through decorators and across the wire, so a
// remote client reports exactly what its server declared.
type Capabilities struct {
	Policy   Enforcement
	Features SessionFeatures
	// Journal is the file-journal surface of this runner instance.
	// The zero value means "no journal": OpenJournal fails with
	// errdefs.NotAvailable. It travels with the rest of Capabilities,
	// and the decorators forward the journal itself so the two never
	// disagree.
	Journal JournalCapabilities
}

// SessionFeatures lists the session capabilities a backend can provide.
// A backend that cannot start sessions at all reports zero values; a
// backend may still return a narrower per-session declaration from
// [Session.Capabilities] (e.g. a pipe session has TTY=false).
type SessionFeatures struct {
	TTY    bool // pty sessions: Resize plus merged TTY output
	Signal bool // SessionSignal delivery
	Events bool // push event streams (Watch)
}
