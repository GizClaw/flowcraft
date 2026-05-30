package pipeline

import (
	"context"
	"time"
)

// DetachCancel returns a context that inherits parent's Values but
// is NOT cancelled when parent is. Compensation paths must run to
// completion even if the inbound RPC was cancelled mid-flight,
// otherwise rollback itself becomes the source of drift:
//
//   - canonical store has appended facts a downstream projection
//     could not absorb, so the pipeline returned an error;
//   - if the rollback inherited the cancelled ctx, store.Delete /
//     fanout.Forget would be cancelled immediately, leaving the
//     ledger in the half-applied state we set out to avoid.
//
// The detached context is value-preserving so OTel trace ids /
// auth metadata still flow into the compensator's downstream
// calls — only cancellation and deadline are severed.
//
// This helper is exported so per-flow stages may build their own
// cleanup ctx in cases where the framework's reverse-stage walk
// is too coarse (e.g. parallel compensation within a single
// stage).
func DetachCancel(parent context.Context) context.Context {
	return detachedCtx{parent: parent}
}

// detachedCtx satisfies context.Context with value pass-through but
// no cancellation / deadline propagation. Done returns a nil
// channel (treated by callers as "never closes") and Err returns
// nil for the same reason.
type detachedCtx struct {
	parent context.Context
}

func (detachedCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (detachedCtx) Done() <-chan struct{}       { return nil }
func (detachedCtx) Err() error                  { return nil }
func (c detachedCtx) Value(key any) any {
	if c.parent == nil {
		return nil
	}
	return c.parent.Value(key)
}
