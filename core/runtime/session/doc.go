// Package session owns a run's transport-neutral lifetime: which sessions
// exist, which turn is executing in each, and how a turn's events reach the
// consumer that subscribed to it. Nothing here knows about a transport
// (HTTP, SSE, gRPC) or about how a turn is driven — an engine runs inside a
// [Turn], and a host-facing adapter consumes its [Sink].
//
// # Layers
//
//	Manager  — the session table, its tunables, and the epoch's Deps
//	Session  — one conversation: sinks, checkpoints, activity bookkeeping
//	Turn     — one execution: its runCtx, its sink subscription, its result
//	queuedSink — one subscriber: ordered delivery, backpressure, detach
//
// [Manager] also swaps the runtime's dependency epoch (Deps: resolver, host
// factory, checkpoint store, catalog) without dropping live sessions. [Turn]
// is started by a Session, driven by whoever holds it, and terminated exactly
// once: completed, interrupted, cancelled, or aborted.
//
// # Invariants
//
// A turn's own runCtx is the parent of every context derived from it, and the
// coordinator that observes the run is subscribed with context.WithoutCancel:
// the subscription is detached explicitly when the turn finishes, so letting a
// cancelled runCtx tear it down would strand the run-end drain until the
// timeout and emit a spurious finalize warning.
//
// Durable state work (checkpoint save, delete, reap) runs on a context that
// drops cancellation and carries a short deadline: a close path must finish
// even when the caller has already given up. Everything else inherits the
// caller's context, so a deadline or a span reaches the work that honoured it.
//
// Subscriber callbacks run on the sink's delivery goroutines, never on the
// caller's: a slow or panicking consumer must not stall or crash the turn. The
// detach callback is guarded the same way, and its panic is recorded rather
// than swallowed.
//
// Delivery is bounded twice — a per-sink in-flight limit and a queue with a
// declared backpressure policy — so a consumer that stops reading degrades
// visibly (a detach with a cause) instead of growing without limit.
//
// # Boundary
//
// The agent contract (Board, Host, Result, Observer) is not modelled here; a
// session drives engines through an Engine and reaches host capabilities
// through a HostFactory. Turn events themselves are event envelopes whose
// schema belongs to the packages that publish them.
package session
