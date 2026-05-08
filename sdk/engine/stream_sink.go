package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// StreamSink is the consumer-side counterpart of the EmitStream*
// helpers. A sink receives one decoded [StreamDeltaPayload] at a
// time along with its source envelope (for headers / trace ids /
// raw subject access) and forwards it to whatever transport the
// caller cares about — SSE, WebSocket, WebRTC datachannel, log
// file, metrics counter, etc.
//
// Implementations:
//   - MUST be safe for concurrent OnDelta calls; the router below
//     fans out to multiple sinks from one goroutine but a custom
//     consumer may use the type from many.
//   - SHOULD return errors only for unrecoverable failures (closed
//     transport, broken pipe). Returned errors propagate to the
//     router's per-sink error log; they do NOT abort delivery to
//     other sinks attached to the same run.
//   - MUST NOT block longer than the transport's natural backoff;
//     long-running work belongs in a worker goroutine that the sink
//     drains into.
//
// [StreamSinkFunc] is the canonical func adapter.
type StreamSink interface {
	OnDelta(ctx context.Context, env event.Envelope, delta StreamDeltaPayload) error
}

// StreamSinkFunc is a func adapter for [StreamSink]. Use it to
// inline a sink without declaring a named type:
//
//	router.Attach(runID, engine.StreamSinkFunc(func(ctx, env, d) error {
//	    return sse.WriteJSON(ctx, d)
//	}))
type StreamSinkFunc func(ctx context.Context, env event.Envelope, delta StreamDeltaPayload) error

// OnDelta implements [StreamSink].
func (f StreamSinkFunc) OnDelta(ctx context.Context, env event.Envelope, delta StreamDeltaPayload) error {
	return f(ctx, env, delta)
}

// StreamRouterOption tunes [NewStreamRouter] behaviour.
type StreamRouterOption func(*streamRouterOpts)

type streamRouterOpts struct {
	bufferSize  int
	subOpts     []event.SubOption
	onSinkError func(sinkID string, err error)
	includeAll  bool // subscribe to PatternRun instead of PatternRunStream
}

// WithStreamBufferSize sets the underlying subscription buffer.
// Default 256 — ample for typical token streams without consuming
// much memory. Pass via [NewStreamRouter] OR per-attachment via
// [WithStreamSubOptions].
func WithStreamBufferSize(n int) StreamRouterOption {
	return func(o *streamRouterOpts) {
		if n > 0 {
			o.bufferSize = n
		}
	}
}

// WithStreamSubOptions appends opaque [event.SubOption] values to
// the router's bus subscription (e.g. WithBackpressure, custom
// predicates). The router prepends WithBufferSize from
// [WithStreamBufferSize] so callers cannot accidentally clobber
// the configured buffer.
func WithStreamSubOptions(opts ...event.SubOption) StreamRouterOption {
	return func(o *streamRouterOpts) {
		o.subOpts = append(o.subOpts, opts...)
	}
}

// WithStreamSinkErrorHandler installs a callback invoked once per
// sink-returned error. Defaults to a no-op (errors silently
// dropped) — observability of sink failures is the caller's
// responsibility.
func WithStreamSinkErrorHandler(fn func(sinkID string, err error)) StreamRouterOption {
	return func(o *streamRouterOpts) {
		if fn != nil {
			o.onSinkError = fn
		}
	}
}

// WithStreamIncludeAllRunEvents switches the router to subscribe
// against [PatternRun] (everything for the run) instead of just
// [PatternRunStream]. Useful for transports that mirror the full
// event log (run.start / step.complete / etc.) rather than just
// stream deltas. When enabled, sinks receive the raw envelope but
// the decoded delta is the zero value for non-stream events;
// consumers should branch on [IsStreamDelta] before reading delta
// fields.
func WithStreamIncludeAllRunEvents() StreamRouterOption {
	return func(o *streamRouterOpts) {
		o.includeAll = true
	}
}

// StreamRouter forwards stream deltas (and optionally the rest of
// a run's lifecycle events) from one [event.Bus] to a dynamic set
// of sinks. It owns one subscription per run and tears it down
// automatically when the run's `engine.run.<id>.end` envelope is
// observed, so callers do not have to thread cleanup through their
// transport layer.
//
// Typical use inside an HTTP handler that streams an SSE response:
//
//	router := engine.NewStreamRouter(bus,
//	    engine.WithStreamSinkErrorHandler(func(id string, err error) {
//	        log.Warn("sink error", "sink", id, "err", err)
//	    }),
//	)
//	defer router.Close()
//	stop, err := router.Attach(runID, "sse-"+reqID, sseSink)
//	if err != nil { ... }
//	defer stop()      // detaches when the request body closes
//
// Multiple sinks may be attached to the same runID concurrently;
// each receives every delta. Attaching to a runID that has not yet
// produced events is fine — the underlying subscription is created
// lazily on first Attach, so the router observes events emitted
// after the call.
type StreamRouter struct {
	bus  event.Bus
	opts streamRouterOpts

	mu      sync.Mutex
	runs    map[string]*runFanout
	closed  bool
	cancel  context.CancelFunc
	rootCtx context.Context
}

type runFanout struct {
	cancel context.CancelFunc
	sub    event.Subscription
	mu     sync.Mutex
	sinks  map[string]StreamSink
	done   chan struct{}
}

// NewStreamRouter constructs a router bound to bus. The router
// holds a single root context whose cancellation tears down every
// active subscription on [Close]. nil bus is rejected.
func NewStreamRouter(bus event.Bus, opts ...StreamRouterOption) *StreamRouter {
	o := streamRouterOpts{bufferSize: 256, onSinkError: func(string, error) {}}
	for _, fn := range opts {
		fn(&o)
	}
	rootCtx, cancel := context.WithCancel(context.Background())
	return &StreamRouter{
		bus:     bus,
		opts:    o,
		runs:    make(map[string]*runFanout),
		rootCtx: rootCtx,
		cancel:  cancel,
	}
}

// Attach registers sink for runID, returning a detach function the
// caller MUST invoke when the transport closes (deferred-friendly).
// sinkID identifies the attachment in error reports; pick something
// stable so logs can be correlated.
//
// Returns an error if the router has been Close-d. Re-attaching a
// previously-detached sinkID is allowed.
func (r *StreamRouter) Attach(runID, sinkID string, sink StreamSink) (detach func(), err error) {
	if sink == nil {
		return nil, errors.New("engine.StreamRouter.Attach: sink is nil")
	}
	if runID == "" || sinkID == "" {
		return nil, errors.New("engine.StreamRouter.Attach: runID and sinkID are required")
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("engine.StreamRouter: closed")
	}
	rf, ok := r.runs[runID]
	if !ok {
		var berr error
		rf, berr = r.spawnFanoutLocked(runID)
		if berr != nil {
			r.mu.Unlock()
			return nil, berr
		}
		r.runs[runID] = rf
	}
	r.mu.Unlock()

	rf.mu.Lock()
	rf.sinks[sinkID] = sink
	rf.mu.Unlock()

	return func() {
		rf.mu.Lock()
		delete(rf.sinks, sinkID)
		empty := len(rf.sinks) == 0
		rf.mu.Unlock()
		if empty {
			r.detachRun(runID)
		}
	}, nil
}

// spawnFanoutLocked creates the bus subscription for runID and
// starts its dispatch loop. Caller MUST hold r.mu.
func (r *StreamRouter) spawnFanoutLocked(runID string) (*runFanout, error) {
	pattern := PatternRunStream(runID)
	if r.opts.includeAll {
		pattern = PatternRun(runID)
	}
	subOpts := append([]event.SubOption{event.WithBufferSize(r.opts.bufferSize)}, r.opts.subOpts...)
	subCtx, cancel := context.WithCancel(r.rootCtx)
	sub, err := r.bus.Subscribe(subCtx, pattern, subOpts...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("engine.StreamRouter: subscribe %s: %w", pattern, err)
	}
	rf := &runFanout{
		cancel: cancel,
		sub:    sub,
		sinks:  make(map[string]StreamSink),
		done:   make(chan struct{}),
	}
	go r.runLoop(runID, rf, subCtx)
	return rf, nil
}

// runLoop drains rf.sub and fans each envelope out to every
// currently-attached sink. It returns when the subscription
// channel closes (bus close, ctx cancel, or run end). The "run
// end" auto-cleanup is implemented by recognising
// [SubjectRunEnd] and cancelling our own subCtx — the subsequent
// channel close terminates the loop naturally.
func (r *StreamRouter) runLoop(runID string, rf *runFanout, subCtx context.Context) {
	defer close(rf.done)
	endSubject := SubjectRunEnd(runID)
	for env := range rf.sub.C() {
		// Decode once per envelope; non-stream events leave delta
		// at zero value, which is fine when WithStreamIncludeAllRunEvents
		// is in effect.
		var delta StreamDeltaPayload
		if IsStreamDelta(env.Subject) {
			if d, err := DecodeStreamDelta(env); err == nil {
				delta = d
			} else {
				r.opts.onSinkError("decode", err)
			}
		}

		// Snapshot sinks under the per-run lock; release before
		// invoking handlers so a slow sink does not block
		// Attach / Detach for siblings.
		rf.mu.Lock()
		snap := make([]struct {
			id   string
			sink StreamSink
		}, 0, len(rf.sinks))
		for id, s := range rf.sinks {
			snap = append(snap, struct {
				id   string
				sink StreamSink
			}{id, s})
		}
		rf.mu.Unlock()

		for _, p := range snap {
			if err := p.sink.OnDelta(subCtx, env, delta); err != nil {
				r.opts.onSinkError(p.id, err)
			}
		}

		if env.Subject == endSubject {
			// Schedule teardown after we have delivered the end
			// event to subscribers — they may want to render a
			// terminal "[done]" frame. The detach removes us
			// from r.runs so any future Attach re-creates a
			// fresh fanout.
			r.detachRun(runID)
		}
	}
}

// detachRun cancels and removes the per-run fanout if present.
// Idempotent.
func (r *StreamRouter) detachRun(runID string) {
	r.mu.Lock()
	rf, ok := r.runs[runID]
	if ok {
		delete(r.runs, runID)
	}
	r.mu.Unlock()
	if ok {
		rf.cancel()
		_ = rf.sub.Close()
	}
}

// Close tears down every active fanout and waits for their loops
// to drain. Subsequent Attach calls return an error. Close is
// idempotent.
func (r *StreamRouter) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	runs := r.runs
	r.runs = nil
	r.mu.Unlock()

	r.cancel()
	for _, rf := range runs {
		_ = rf.sub.Close()
		<-rf.done
	}
	return nil
}
