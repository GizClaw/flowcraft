package session

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

const defaultAttemptDrainTimeout = 5 * time.Second

type branchKey struct {
	forkID   string
	branchID string
}

type branchState struct {
	terminal agent.StreamDeltaType
	items    []sinkItem
	bytes    int
}

// streamCoordinator is the turn's sole event.Router attachment: it
// decodes stream-delta envelopes, observes run-end subjects to delimit
// attempts, and fans confirmed/raw deltas out to the turn's sinks.
type streamCoordinator struct {
	turn      *Turn
	runEnd    event.Subject
	raw       []*queuedSink
	confirmed []*queuedSink
	maxEvents int
	maxBytes  int

	mu            sync.Mutex
	branches      map[branchKey]*branchState
	pendingEvents int
	pendingBytes  int
	nextCursor    DeliveryCursor
	attempt       int
	confirmedDead bool
	finalized     bool
	attemptEnded  chan struct{}
}

func newStreamCoordinator(turn *Turn, raw, confirmed []*queuedSink, maxEvents, maxBytes int) *streamCoordinator {
	return &streamCoordinator{
		turn: turn, runEnd: agent.SubjectRunEnd(turn.runID),
		raw: raw, confirmed: confirmed, maxEvents: maxEvents, maxBytes: maxBytes,
		branches: make(map[branchKey]*branchState), attempt: 1,
		attemptEnded: make(chan struct{}, 1),
	}
}

// OnEnvelope implements event.Sink: run-end subjects delimit attempts,
// stream-delta subjects are decoded, and every other run event is
// forwarded as an empty delta — matching the legacy router's
// include-all-run-events behaviour, which raw sinks rely on as a
// "the run is alive" signal.
func (c *streamCoordinator) OnEnvelope(ctx context.Context, env event.Envelope) error {
	if env.Subject == c.runEnd {
		return c.OnDelta(ctx, env, agent.StreamDeltaPayload{})
	}
	if agent.IsStreamDelta(env.Subject) {
		delta, err := agent.DecodeStreamDelta(env)
		if err != nil {
			return err
		}
		return c.OnDelta(ctx, env, delta)
	}
	return c.OnDelta(ctx, env, agent.StreamDeltaPayload{})
}

func (c *streamCoordinator) OnDelta(ctx context.Context, env event.Envelope, delta agent.StreamDeltaPayload) error {
	c.mu.Lock()
	if c.finalized {
		c.mu.Unlock()
		return nil
	}
	// Engine run-end events delimit attempts, not the logical turn. The
	// EventBus preserves subscription order, so incrementing here makes
	// every subsequently observed event part of the next attempt.
	if env.Subject == c.runEnd {
		c.finishAttemptLocked()
		c.attempt++
		select {
		case c.attemptEnded <- struct{}{}:
		default:
		}
		c.mu.Unlock()
		return nil
	}

	// queuedSink.OnDelta never returns a non-nil error: a full queue
	// detaches the sink instead of surfacing one, so fan-out here is
	// intentionally fire-and-forget.
	for _, sink := range c.raw {
		_ = sink.OnDelta(ctx, env, delta)
	}

	if c.confirmedDead {
		c.mu.Unlock()
		return nil
	}

	key := branchKey{forkID: delta.ForkID, branchID: delta.BranchID}
	control := delta.Type == agent.StreamDeltaParallelBranchAccept ||
		delta.Type == agent.StreamDeltaParallelBranchCancel
	if (delta.Speculative || control) && (key.forkID == "" || key.branchID == "") {
		c.failConfirmedLocked(errdefs.Validationf(
			"runtime session: speculative stream identity requires ForkID and BranchID"))
		c.mu.Unlock()
		return nil
	}
	switch {
	case delta.Speculative:
		state := c.branches[key]
		if state == nil {
			if len(c.branches) >= c.maxEvents {
				c.failConfirmedLocked(errdefs.BudgetExceededf(
					"runtime session: speculative branch state limit exceeded"))
				c.mu.Unlock()
				return nil
			}
			state = &branchState{}
			c.branches[key] = state
		}
		if state.terminal != "" {
			c.failConfirmedLocked(errdefs.Conflictf(
				"runtime session: speculative data after terminal for fork %q branch %q",
				key.forkID, key.branchID))
			c.mu.Unlock()
			return nil
		}
		size := len(env.Payload)
		if c.pendingEvents+1 > c.maxEvents || c.pendingBytes+size > c.maxBytes {
			c.failConfirmedLocked(errdefs.BudgetExceededf(
				"runtime session: speculative buffer limit exceeded"))
			c.mu.Unlock()
			return nil
		}
		state.items = append(state.items, sinkItem{ctx: context.WithoutCancel(ctx), env: env, delta: delta})
		state.bytes += size
		c.pendingEvents++
		c.pendingBytes += size
	case control:
		state := c.branches[key]
		if state == nil {
			if len(c.branches) >= c.maxEvents {
				c.failConfirmedLocked(errdefs.BudgetExceededf(
					"runtime session: speculative branch state limit exceeded"))
				c.mu.Unlock()
				return nil
			}
			state = &branchState{}
			c.branches[key] = state
		}
		if state.terminal == delta.Type {
			c.mu.Unlock()
			return nil
		}
		if state.terminal != "" {
			c.failConfirmedLocked(errdefs.Conflictf(
				"runtime session: conflicting speculative terminal for fork %q branch %q",
				key.forkID, key.branchID))
			c.mu.Unlock()
			return nil
		}
		state.terminal = delta.Type
		c.emitConfirmedLocked(ctx, env, delta)
		if delta.Type == agent.StreamDeltaParallelBranchAccept {
			for _, item := range state.items {
				c.emitConfirmedLocked(item.ctx, item.env, item.delta)
			}
		}
		c.pendingEvents -= len(state.items)
		c.pendingBytes -= state.bytes
		state.items = nil
		state.bytes = 0
	default:
		c.emitConfirmedLocked(ctx, env, delta)
	}
	c.mu.Unlock()
	return nil
}

var _ event.Sink = (*streamCoordinator)(nil)

func (c *streamCoordinator) emitConfirmedLocked(ctx context.Context, env event.Envelope, delta agent.StreamDeltaPayload) {
	c.nextCursor++
	cursor := c.nextCursor
	cloned := env
	cloned.Headers = make(map[string]string, len(env.Headers)+1)
	for key, value := range env.Headers {
		cloned.Headers[key] = value
	}
	cloned.Headers[HeaderDeliveryCursor] = strconv.FormatUint(uint64(cursor), 10)
	c.turn.recordConfirmed(c.attempt, cursor, delta)
	// See the raw fan-out above: OnDelta is enqueue-or-detach and
	// always reports nil.
	for _, sink := range c.confirmed {
		_ = sink.OnDelta(ctx, cloned, delta)
	}
}

func (c *streamCoordinator) finishAttemptLocked() {
	for key, state := range c.branches {
		if state.terminal == "" {
			c.failConfirmedLocked(errdefs.Conflictf(
				"runtime session: run ended before speculative terminal for fork %q branch %q",
				key.forkID, key.branchID))
			return
		}
	}
	c.branches = make(map[branchKey]*branchState)
	c.pendingEvents, c.pendingBytes = 0, 0
}

type logicalRunEndPayload struct {
	Status agent.Status `json:"status,omitempty"`
	Error  string       `json:"error,omitempty"`
}

// finalize emits the one externally visible logical run end. It is
// idempotent because finish and shutdown paths may converge.
func (c *streamCoordinator) finalize(ctx context.Context, result *agent.Result, runErr error) error {
	// With no raw or confirmed sinks there is nothing to drain and no
	// consumer for the logical run end, so the attempt wait is pure
	// overhead: an engine that never publishes run-end would otherwise
	// stall the turn by the full drain budget with no observable effect.
	if len(c.raw) == 0 && len(c.confirmed) == 0 {
		return nil
	}
	expectedAttempts := 1
	if result != nil && result.Attempts > 0 {
		expectedAttempts = result.Attempts
	}
	for {
		c.mu.Lock()
		ready := c.attempt > expectedAttempts
		alreadyFinalized := c.finalized
		c.mu.Unlock()
		if ready || alreadyFinalized {
			break
		}
		select {
		case <-c.attemptEnded:
		case <-ctx.Done():
			return errdefs.Timeout(fmt.Errorf(
				"runtime session: wait for attempt stream drain: %w", ctx.Err()))
		}
	}

	payload := logicalRunEndPayload{}
	if result != nil {
		payload.Status = result.Status
		if result.Err != nil {
			payload.Error = result.Err.Error()
		}
	}
	if payload.Error == "" && runErr != nil {
		payload.Error = runErr.Error()
	}
	env := event.MustEnvelope(ctx, c.runEnd, payload)
	env.SetRunID(c.turn.runID)

	c.mu.Lock()
	if c.finalized {
		c.mu.Unlock()
		return nil
	}
	c.finalized = true
	c.finishAttemptLocked()
	confirmedLive := !c.confirmedDead
	c.mu.Unlock()

	// Raw fan-out carries the same enqueue-or-detach contract as
	// OnDelta above.
	for _, sink := range c.raw {
		_ = sink.OnDelta(ctx, env, agent.StreamDeltaPayload{})
	}
	if !confirmedLive {
		return nil
	}

	c.mu.Lock()
	if !c.confirmedDead {
		c.emitConfirmedLocked(ctx, env, agent.StreamDeltaPayload{})
	}
	c.mu.Unlock()
	return nil
}

func (c *streamCoordinator) failConfirmedLocked(err error) {
	if c.confirmedDead {
		return
	}
	telemetry.WarnErr(
		context.WithoutCancel(c.turn.runCtx),
		"runtime session: confirmed stream failed and detached",
		err,
		otellog.String(telemetry.AttrRunID, c.turn.runID),
	)
	c.confirmedDead = true
	c.branches = nil
	c.pendingEvents, c.pendingBytes = 0, 0
	for _, sink := range c.confirmed {
		sink.detach(err)
	}
}
