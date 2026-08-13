package sandbox

import (
	"context"
)

// Runner is a sandbox: it starts interactive process sessions under the
// sandbox's policy and manages their lifecycle. The one-shot form
// [Exec] is a derived view over Start. Implementations MUST honour
// ExecOptions.Timeout and reject any policy they cannot enforce with
// an errdefs.NotAvailable error rather than silently downgrading the
// request.
//
// Capabilities is mandatory: it is the explicit, wire-safe declaration
// of what this backend can do. Callers and tools never discover
// features through interface assertions — unsupported operations on a
// returned [Session] fail with errdefs.NotAvailable, and Capabilities
// says up front why.
type Runner interface {
	Capabilities() Capabilities
	Start(ctx context.Context, spec SessionSpec) (Session, error)
	List(ctx context.Context) ([]SessionInfo, error)
	Terminate(ctx context.Context, id string) error
}

// Capabilities is the explicit declaration of a Runner's surface: what
// policy it can enforce and which session features it can provide. It
// travels unchanged through decorators and across the wire, so a
// remote client reports exactly what its server declared.
type Capabilities struct {
	Policy   Enforcement
	Features SessionFeatures
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
