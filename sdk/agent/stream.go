package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------- StreamDelta emit ----------

// Stream-delta emission helpers
// -----------------------------
//
// SubjectStreamDelta + StreamDeltaPayload are the SDK-wide SPI that
// in-flight increments — assistant tokens, tool calls, tool results —
// flow through. Anyone with a [Publisher] can emit them; the contract is
// not LLM-specific. These helpers package the boilerplate (envelope
// construction, well-known headers, payload validation) so a custom
// node, a wrapper engine, or a test harness can publish a valid stream
// delta in a single line:
//
//	// Inside a custom long-running graph node:
//	engine.EmitStreamToken(ctx, pub, runID, nodeID, "loaded chunk 3/10")
//
// They are sugar over [SubjectStreamDelta] + [event.NewEnvelope] —
// callers that need fine-grained control (custom headers, batched
// publish) can still construct the envelope by hand. The helpers do
// nothing if pub is nil so a node that lost its Publisher (e.g. a
// host built with NoopHost{}) keeps running.

// EmitStreamToken publishes one assistant-token delta on the canonical
// stream subject. See [EmitStreamDelta] for the stepActor format
// requirement.
//
// Use this from any node that produces incremental textual output —
// for example a custom RAG retriever streaming its working notes, or a
// post-processing node turning structured data into prose. content may
// be empty (callers that want "still alive" heartbeats should typically
// mark them differently); empty content is published as-is so the
// helper stays predictable.
func EmitStreamToken(ctx context.Context, pub Publisher, runID, stepActor, content string) error {
	return EmitStreamDelta(ctx, pub, runID, stepActor, StreamDeltaPayload{
		Type:    StreamDeltaToken,
		Content: content,
	})
}

// EmitStreamToolCall publishes one tool-call delta. id and name are
// required (consumers correlate the eventual tool_result by ID); args
// is the tool input the model produced and may be either a JSON string
// or an already-decoded map / slice — both are valid per the
// [StreamDeltaPayload] contract. See [EmitStreamDelta] for the
// stepActor format requirement.
//
// The helper validates the required fields up-front and returns a
// descriptive error instead of publishing a malformed envelope; callers
// that already validated upstream can ignore the error safely.
func EmitStreamToolCall(ctx context.Context, pub Publisher, runID, stepActor, id, name string, args any) error {
	if id == "" {
		return fmt.Errorf("engine: EmitStreamToolCall: id is required")
	}
	if name == "" {
		return fmt.Errorf("engine: EmitStreamToolCall: name is required")
	}
	return EmitStreamDelta(ctx, pub, runID, stepActor, StreamDeltaPayload{
		Type:      StreamDeltaToolCall,
		ID:        id,
		Name:      name,
		Arguments: args,
	})
}

// EmitStreamToolResult publishes one tool-result delta. toolCallID and
// content are required (toolCallID pairs the result with the originating
// tool_call); name is recommended so consumers can render the result
// without a separate dispatch table. isError marks unsuccessful
// results; cancelled marks synthesised cancellations emitted when the
// round was interrupted before the call dispatched. See
// [EmitStreamDelta] for the stepActor format requirement.
func EmitStreamToolResult(ctx context.Context, pub Publisher, runID, stepActor, toolCallID, name, content string, isError, cancelled bool) error {
	if toolCallID == "" {
		return fmt.Errorf("engine: EmitStreamToolResult: toolCallID is required")
	}
	return EmitStreamDelta(ctx, pub, runID, stepActor, StreamDeltaPayload{
		Type:       StreamDeltaToolResult,
		ToolCallID: toolCallID,
		Name:       name,
		Content:    content,
		IsError:    isError,
		Cancelled:  cancelled,
	})
}

// EmitStreamDelta is the low-level form of the EmitStreamX helpers.
// Custom nodes that need to set fields outside the type-specific
// helpers (e.g. a forward-compatible Type the SDK does not yet ship a
// helper for) build the payload themselves and pass it here. Required
// per-Type fields are validated to mirror the contract enforced by
// [DecodeStreamDelta] on the consumer side, so a malformed delta is
// caught at publish time instead of silently flowing to subscribers.
//
// stepActor follows the contract documented at the top of subjects.go:
// it MUST start with the executing agent.id (so [PatternRunAgentStream]
// can fan-in by agent) and MAY append an engine-private suffix
// (graph runner: ".node.<nodeID>"; iterative engine: ".iter<N>"). Both
// runID and stepActor are sanitised by [SanitiseID] so caller-supplied
// values cannot fragment the resulting subject.
//
// The envelope is stamped with HeaderRunID. The agent identifier is
// derived from the stepActor segment ahead of any optional ".node." /
// ".iter" suffix — it goes onto HeaderAgentID. For header-routed
// subscribers that key off the node id, the HeaderNodeID is populated
// whenever stepActor carries the graph runner's
// "<agent>.node.<nodeID>" form so the two transports stay aligned.
//
// Publish errors are returned to the caller (unlike the executor's
// fire-and-forget convention) so node authors can decide whether to
// retry or surface the failure; in practice most callers just discard
// the error because stream deltas are observability, not control flow.
func EmitStreamDelta(ctx context.Context, pub Publisher, runID, stepActor string, payload StreamDeltaPayload) error {
	if pub == nil {
		return nil
	}
	if err := validateStreamDelta(payload); err != nil {
		return err
	}
	subject := SubjectStreamDelta(runID, stepActor)
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return err
	}
	if runID != "" {
		env.SetRunID(runID)
	}
	agentID, nodeID := splitStepActor(stepActor)
	if agentID != "" {
		env.SetAgentID(agentID)
	}
	if nodeID != "" {
		env.SetNodeID(nodeID)
	}
	return pub.Publish(ctx, env)
}

// splitStepActor extracts the agent.id prefix and the optional graph
// runner ".node.<nodeID>" suffix from a stepActor string. Returns
// (stepActor, "") when no recognised suffix is present, so engines
// that use a different suffix scheme (e.g. an iterative engine's ".iter<N>")
// only get the agent.id projected onto HeaderAgentID and rely on
// other facilities for the rest.
//
// Kept private because the suffix vocabulary is not part of any
// consumer-facing contract — only the agent.id prefix is.
func splitStepActor(stepActor string) (agentID, nodeID string) {
	const nodeSep = ".node."
	for i := 0; i+len(nodeSep) <= len(stepActor); i++ {
		if stepActor[i:i+len(nodeSep)] == nodeSep {
			return stepActor[:i], stepActor[i+len(nodeSep):]
		}
	}
	return stepActor, ""
}

// validateStreamDelta mirrors the per-Type field requirements
// documented on [StreamDeltaPayload]. We deliberately do NOT validate
// unknown Type values: the contract says consumers SHOULD treat
// unknowns as forward-compatible, so the helper does the same on the
// emit side.
func validateStreamDelta(p StreamDeltaPayload) error {
	switch p.Type {
	case StreamDeltaToken:
		// Content is allowed to be empty — see EmitStreamToken docs.
		return validateStreamDataIdentity(p)
	case StreamDeltaToolCall:
		if p.ID == "" {
			return fmt.Errorf("engine: stream delta tool_call requires ID")
		}
		if p.Name == "" {
			return fmt.Errorf("engine: stream delta tool_call requires Name")
		}
		return validateStreamDataIdentity(p)
	case StreamDeltaToolResult:
		if p.ToolCallID == "" {
			return fmt.Errorf("engine: stream delta tool_result requires ToolCallID")
		}
		return validateStreamDataIdentity(p)
	case StreamDeltaParallelBranchAccept, StreamDeltaParallelBranchCancel:
		if p.ForkID == "" {
			return fmt.Errorf("engine: stream delta %s requires ForkID", p.Type)
		}
		if p.BranchID == "" {
			return fmt.Errorf("engine: stream delta %s requires BranchID", p.Type)
		}
		return nil
	case "":
		return fmt.Errorf("engine: stream delta requires Type")
	default:
		// Forward-compatible Type — accept it.
		return nil
	}
}

// validateStreamDataIdentity keeps ordinary and speculative data deltas
// unambiguous. Speculative branch output always carries both correlation
// identifiers; non-speculative output carries neither.
func validateStreamDataIdentity(p StreamDeltaPayload) error {
	if p.Speculative {
		if p.ForkID == "" {
			return fmt.Errorf("engine: speculative stream delta %s requires ForkID", p.Type)
		}
		if p.BranchID == "" {
			return fmt.Errorf("engine: speculative stream delta %s requires BranchID", p.Type)
		}
		return nil
	}
	if p.ForkID != "" || p.BranchID != "" {
		return fmt.Errorf(
			"engine: non-speculative stream delta %s must not carry ForkID or BranchID",
			p.Type)
	}
	return nil
}

// ---------- Stream router / sinks ----------

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
//   - MUST observe ctx.Done and return promptly. Router and graph sink
//     timeouts are bounded only for implementations that honor this
//     cooperative cancellation contract.
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

// AttachOption tunes one [StreamRouter.Attach] attachment.
type AttachOption func(*streamAttachOpts)

type streamAttachOpts struct {
	retainAfterRunEnd bool
}

// WithStreamRetainAfterRunEnd keeps an attachment subscribed after a
// SubjectRunEnd envelope. This is intended for one logical run whose
// revise attempts reuse a RunID. The caller must invoke the returned
// detach function after the logical run finishes.
func WithStreamRetainAfterRunEnd() AttachOption {
	return func(o *streamAttachOpts) {
		o.retainAfterRunEnd = true
	}
}

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
// of sinks. It owns one subscription per run. By default an
// attachment is removed when the run's `engine.run.<id>.end`
// envelope is observed; [WithStreamRetainAfterRunEnd] keeps selected
// attachments alive across attempt boundaries.
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
	sinks  map[string]*streamAttachment
	done   chan struct{}
}

type streamAttachment struct {
	sink              StreamSink
	retainAfterRunEnd bool
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
func (r *StreamRouter) Attach(
	runID, sinkID string,
	sink StreamSink,
	opts ...AttachOption,
) (detach func(), err error) {
	if sink == nil {
		return nil, errors.New("engine.StreamRouter.Attach: sink is nil")
	}
	if runID == "" || sinkID == "" {
		return nil, errors.New("engine.StreamRouter.Attach: runID and sinkID are required")
	}

	var attachOpts streamAttachOpts
	for _, opt := range opts {
		if opt != nil {
			opt(&attachOpts)
		}
	}
	attachment := &streamAttachment{
		sink:              sink,
		retainAfterRunEnd: attachOpts.retainAfterRunEnd,
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
	rf.mu.Lock()
	rf.sinks[sinkID] = attachment
	rf.mu.Unlock()
	r.mu.Unlock()

	return func() {
		rf.mu.Lock()
		if rf.sinks[sinkID] == attachment {
			delete(rf.sinks, sinkID)
		}
		empty := len(rf.sinks) == 0
		rf.mu.Unlock()
		if empty {
			r.detachRunIfEmpty(runID, rf)
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
		sinks:  make(map[string]*streamAttachment),
		done:   make(chan struct{}),
	}
	go r.runLoop(runID, rf, subCtx)
	return rf, nil
}

// runLoop drains rf.sub and fans each envelope out to every
// currently-attached sink. It returns when the subscription channel
// closes. On [SubjectRunEnd], default attachments are removed while
// retained attachments keep the subscription alive.
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
			id         string
			attachment *streamAttachment
		}, 0, len(rf.sinks))
		for id, attachment := range rf.sinks {
			snap = append(snap, struct {
				id         string
				attachment *streamAttachment
			}{id, attachment})
		}
		rf.mu.Unlock()

		for _, p := range snap {
			if err := p.attachment.sink.OnDelta(subCtx, env, delta); err != nil {
				r.opts.onSinkError(p.id, err)
			}
		}

		if env.Subject == endSubject {
			rf.mu.Lock()
			for _, p := range snap {
				if !p.attachment.retainAfterRunEnd &&
					rf.sinks[p.id] == p.attachment {
					delete(rf.sinks, p.id)
				}
			}
			empty := len(rf.sinks) == 0
			rf.mu.Unlock()
			if empty {
				r.detachRunIfEmpty(runID, rf)
			}
		}
	}
}

// detachRunIfEmpty cancels and removes target only when it is still the
// current fanout and no attachment arrived after the caller's empty check.
func (r *StreamRouter) detachRunIfEmpty(runID string, target *runFanout) {
	r.mu.Lock()
	rf, ok := r.runs[runID]
	if ok && rf == target {
		rf.mu.Lock()
		if len(rf.sinks) == 0 {
			delete(r.runs, runID)
		} else {
			ok = false
		}
		rf.mu.Unlock()
	} else {
		ok = false
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
