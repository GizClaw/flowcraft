package engine

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Forwarder is the base for a decorator that changes one thing and forwards the
// rest: it implements [sandbox.Runner] plus the optional capabilities —
// [sandbox.JournalProvider.OpenJournal] and [sandbox.SessionAttacher.Attach] —
// by delegating to Inner.
//
// Embed it and override only what you change:
//
//	type audited struct {
//		*engine.Forwarder
//		sink Sink
//	}
//
//	func (a *audited) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
//		a.sink.Record(spec.Argv)
//		return a.Forwarder.Start(ctx, spec)
//	}
//
// Embedding sandbox.Runner directly is the trap this type exists for: the
// interface does not declare the optional capabilities, so embedding it does
// not promote them and the decorator silently loses the file journal (while
// Capabilities keeps advertising it) and the ability to re-attach a session.
//
// Forwarder deliberately does not implement [sandbox.Executor]: a decorator that
// does not care about the one-shot form keeps the derived view over Start, and
// only a decorator that overrides Exec opts into the override. The flip side is
// that wrapping an Executor in a plain Forwarder takes the override away without
// an error — an escalation runner must stay the outermost value.
type Forwarder struct {
	// Inner is the runner every forwarded call reaches. It is never nil for a
	// value produced by [NewForwarder].
	Inner sandbox.Runner
}

// NewForwarder returns a Forwarder over inner, which must not be nil: a
// decorator that cannot name what it decorates is a construction error, not a
// runner.
func NewForwarder(inner sandbox.Runner) (*Forwarder, error) {
	if inner == nil {
		return nil, errdefs.Validationf("sandbox: forwarder requires an inner runner")
	}
	return &Forwarder{Inner: inner}, nil
}

// Close implements [sandbox.Runner] by forwarding to Inner.
func (f *Forwarder) Close() error { return f.Inner.Close() }

// Capabilities implements [sandbox.Runner] by forwarding Inner's declaration.
func (f *Forwarder) Capabilities() sandbox.Capabilities { return f.Inner.Capabilities() }

// Start implements [sandbox.Runner] by forwarding to Inner.
func (f *Forwarder) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	return f.Inner.Start(ctx, spec)
}

// List implements [sandbox.Runner] by forwarding to Inner.
func (f *Forwarder) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
	return f.Inner.List(ctx)
}

// Terminate implements [sandbox.Runner] by forwarding to Inner.
func (f *Forwarder) Terminate(ctx context.Context, id string) error {
	return f.Inner.Terminate(ctx, id)
}

// OpenJournal implements [sandbox.JournalProvider] by asking Inner for its
// journal: a decorator that changes nothing about the file boundary exposes
// exactly what its inner runner exposes, and NotAvailable when there is none.
func (f *Forwarder) OpenJournal(ctx context.Context) (sandbox.FileJournal, error) {
	return sandbox.OpenJournal(ctx, f.Inner)
}

// Attach implements [sandbox.SessionAttacher] by asking Inner for the handle:
// re-attaching is a transport capability of the backend, and a decorator that
// forwards Start must not take it away.
func (f *Forwarder) Attach(ctx context.Context, id string) (sandbox.Session, error) {
	return sandbox.Attach(ctx, f.Inner, id)
}

var (
	_ sandbox.Runner          = (*Forwarder)(nil)
	_ sandbox.JournalProvider = (*Forwarder)(nil)
	_ sandbox.SessionAttacher = (*Forwarder)(nil)
)
