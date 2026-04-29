package engine

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// HostMiddleware decorates a Host with policy / observability /
// resource-management behaviour. The pod / agent layer typically
// stacks several middlewares (audit → rate-limit → budget →
// secret-resolve) around a base Host built from a PodSpec; this
// type and the [ComposeHost] helper exist so the stack can be
// declared as a slice instead of N levels of struct embedding.
//
// Convention:
//
//   - Middleware ordering matches the slice order: ComposeHost(base,
//     A, B, C) returns C(B(A(base))). The first middleware in the
//     slice is the OUTERMOST wrapper and therefore runs first when an
//     engine calls a Host method. This matches how HTTP middleware
//     stacks are normally declared.
//   - Each middleware MUST return a Host value that delegates
//     unchanged for any sub-interface it does not specifically
//     decorate. The [HostFuncs] adapter is provided to make this
//     easy: zero-value func fields delegate to the wrapped Host.
//   - Middlewares are invoked from any goroutine — implementations
//     must be safe for concurrent use, mirroring the Host contract.
type HostMiddleware func(Host) Host

// ComposeHost returns base wrapped by every middleware in mws, in
// declaration order (first = outermost). Returns base unchanged when
// mws is empty.
func ComposeHost(base Host, mws ...HostMiddleware) Host {
	// Apply in reverse so the first slice entry ends up as the
	// outermost wrapper. ComposeHost(base, A, B) ≡ A(B(base)) so a
	// caller reading the slice top-down sees "A first, then B".
	h := base
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] == nil {
			continue
		}
		h = mws[i](h)
		if h == nil {
			// A middleware that returns nil would silently break the
			// chain at the next call. Refuse the whole compose so the
			// programming bug surfaces immediately at assembly time.
			panic("engine.ComposeHost: middleware returned nil Host")
		}
	}
	return h
}

// HostFuncs is the func-field adapter that lets a middleware decorate
// just the Host methods it cares about while delegating the rest to
// an underlying Host. Construct one with the inner host as Inner and
// override only the func fields you need:
//
//	wrapped := engine.HostFuncs{
//	    Inner: base,
//	    ReportUsageFn: func(ctx context.Context, u model.TokenUsage) error {
//	        // budget enforcement here
//	        return base.ReportUsage(ctx, u)
//	    },
//	}
//
// Every nil func field falls through to Inner so partial decorators
// stay short. Inner MUST be non-nil; a nil Inner is a programming
// bug and triggers a panic at the first delegated call.
type HostFuncs struct {
	Inner Host

	PublishFn     func(ctx context.Context, env event.Envelope) error
	InterruptsFn  func() <-chan Interrupt
	AskUserFn     func(ctx context.Context, prompt UserPrompt) (UserReply, error)
	CheckpointFn  func(ctx context.Context, cp Checkpoint) error
	ReportUsageFn func(ctx context.Context, usage model.TokenUsage) error
}

// Publish routes through PublishFn or Inner.
func (h HostFuncs) Publish(ctx context.Context, env event.Envelope) error {
	if h.PublishFn != nil {
		return h.PublishFn(ctx, env)
	}
	return h.requireInner().Publish(ctx, env)
}

// Interrupts routes through InterruptsFn or Inner.
func (h HostFuncs) Interrupts() <-chan Interrupt {
	if h.InterruptsFn != nil {
		return h.InterruptsFn()
	}
	return h.requireInner().Interrupts()
}

// AskUser routes through AskUserFn or Inner.
func (h HostFuncs) AskUser(ctx context.Context, prompt UserPrompt) (UserReply, error) {
	if h.AskUserFn != nil {
		return h.AskUserFn(ctx, prompt)
	}
	return h.requireInner().AskUser(ctx, prompt)
}

// Checkpoint routes through CheckpointFn or Inner.
func (h HostFuncs) Checkpoint(ctx context.Context, cp Checkpoint) error {
	if h.CheckpointFn != nil {
		return h.CheckpointFn(ctx, cp)
	}
	return h.requireInner().Checkpoint(ctx, cp)
}

// ReportUsage routes through ReportUsageFn or Inner.
func (h HostFuncs) ReportUsage(ctx context.Context, usage model.TokenUsage) error {
	if h.ReportUsageFn != nil {
		return h.ReportUsageFn(ctx, usage)
	}
	return h.requireInner().ReportUsage(ctx, usage)
}

// requireInner panics with a clear message when a delegated method
// is invoked without an Inner host configured. Caught at the first
// call rather than producing a confusing nil-pointer trace several
// frames in.
func (h HostFuncs) requireInner() Host {
	if h.Inner == nil {
		panic("engine.HostFuncs: Inner is nil; cannot delegate")
	}
	return h.Inner
}
