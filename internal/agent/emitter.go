// Package agent contains the producer-side helpers for emitting agent.* events.
//
// Emitter is the canonical entry point: agent runtimes call Emitter.RunStarted
// when a card begins execution, then Emitter.Stream for streaming tokens, and
// finally Emitter.RunCompleted / Emitter.RunFailed when the run terminates.
//
// Stream deltas are batched by DeltaFlusher to avoid an envelope-per-token
// flood: deltas are accumulated in memory until either the buffer reaches
// MaxBatchBytes (default 512 B) or MaxBatchInterval (default 100 ms) elapses.
package agent

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// Emitter publishes agent.* envelopes into the eventlog.
//
// Each method opens its own short-lived Atomic transaction so callers do not
// have to thread a UnitOfWork. Atomic is chosen (rather than direct Append)
// because the bus subscriber wake-up is in-tx, ensuring transports see the
// new envelope as soon as Atomic returns.
type Emitter struct {
	log eventlog.Log
}

// NewEmitter constructs an Emitter.
func NewEmitter(log eventlog.Log) *Emitter {
	return &Emitter{log: log}
}

// RunStarted emits agent.run.started.
func (e *Emitter) RunStarted(ctx context.Context, cardID, runID string, actor eventlog.Actor) error {
	_, err := e.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishAgentRunStartedInTx(ctx, uow, cardID, eventlog.AgentRunStartedPayload{
			CardID: cardID,
			RunID:  runID,
		}, eventlog.WithActor(actor))
	})
	return err
}

// RunCompleted emits agent.run.completed.
func (e *Emitter) RunCompleted(ctx context.Context, cardID, runID, output string, actor eventlog.Actor) error {
	_, err := e.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishAgentRunCompletedInTx(ctx, uow, cardID, eventlog.AgentRunCompletedPayload{
			CardID: cardID,
			RunID:  runID,
			Output: output,
		}, eventlog.WithActor(actor))
	})
	return err
}

// RunFailed emits agent.run.failed.
func (e *Emitter) RunFailed(ctx context.Context, cardID, runID, errMsg string, actor eventlog.Actor) error {
	_, err := e.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishAgentRunFailedInTx(ctx, uow, cardID, eventlog.AgentRunFailedPayload{
			CardID: cardID,
			RunID:  runID,
			Error:  errMsg,
		}, eventlog.WithActor(actor))
	})
	return err
}

// ToolInvoked emits agent.tool.invoked.
func (e *Emitter) ToolInvoked(ctx context.Context, cardID, runID, toolName, callID, arguments string, actor eventlog.Actor) error {
	_, err := e.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishAgentToolInvokedInTx(ctx, uow, cardID, eventlog.AgentToolInvokedPayload{
			CardID:    cardID,
			RunID:     runID,
			ToolName:  toolName,
			CallID:    callID,
			Arguments: arguments,
		}, eventlog.WithActor(actor))
	})
	return err
}

// ToolReturned emits agent.tool.returned.
func (e *Emitter) ToolReturned(ctx context.Context, cardID, runID, toolName, callID, status, output, errMsg string, durationMs int64, actor eventlog.Actor) error {
	_, err := e.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return eventlog.PublishAgentToolReturnedInTx(ctx, uow, cardID, eventlog.AgentToolReturnedPayload{
			CardID:     cardID,
			RunID:      runID,
			ToolName:   toolName,
			CallID:     callID,
			Status:     status,
			Output:     output,
			Error:      errMsg,
			DurationMs: durationMs,
		}, eventlog.WithActor(actor))
	})
	return err
}

// FlusherOptions tunes the DeltaFlusher behavior.
type FlusherOptions struct {
	MaxBatchBytes    int           // default 512
	MaxBatchInterval time.Duration // default 100ms
}

// DeltaFlusher batches Push calls into agent.stream.delta envelopes.
//
// One DeltaFlusher per run/role. Push is non-blocking; the batch is published
// on the next interval tick or when MaxBatchBytes is exceeded. Close drains
// any remaining buffer.
type DeltaFlusher struct {
	emitter *Emitter
	cardID  string
	runID   string
	role    string // "" for assistant, "thinking" for thinking trace
	convoID string
	actor   eventlog.Actor
	opts    FlusherOptions

	deltaSeq atomic.Int64
	mu       sync.Mutex
	buf      []byte
	timer    *time.Timer
	closed   bool
}

// NewDeltaFlusher constructs a flusher emitting agent.stream.delta.
func NewDeltaFlusher(emitter *Emitter, cardID, runID, role, convoID string, actor eventlog.Actor, opts FlusherOptions) *DeltaFlusher {
	if opts.MaxBatchBytes <= 0 {
		opts.MaxBatchBytes = 512
	}
	if opts.MaxBatchInterval <= 0 {
		opts.MaxBatchInterval = 100 * time.Millisecond
	}
	return &DeltaFlusher{
		emitter: emitter,
		cardID:  cardID,
		runID:   runID,
		role:    role,
		convoID: convoID,
		actor:   actor,
		opts:    opts,
	}
}

// Push appends the chunk to the batch buffer. Safe to call from any goroutine.
func (f *DeltaFlusher) Push(ctx context.Context, chunk string) {
	if chunk == "" {
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.buf = append(f.buf, chunk...)
	overflow := len(f.buf) >= f.opts.MaxBatchBytes
	if f.timer == nil {
		f.timer = time.AfterFunc(f.opts.MaxBatchInterval, func() {
			f.flush(context.Background(), false)
		})
	}
	f.mu.Unlock()
	if overflow {
		f.flush(ctx, false)
	}
}

// Close flushes any remaining buffer and emits a final delta with finished=true.
func (f *DeltaFlusher) Close(ctx context.Context) {
	f.flush(ctx, true)
}

func (f *DeltaFlusher) flush(ctx context.Context, finished bool) {
	f.mu.Lock()
	if f.closed && !finished {
		f.mu.Unlock()
		return
	}
	if finished {
		f.closed = true
	}
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	chunk := string(f.buf)
	f.buf = f.buf[:0]
	f.mu.Unlock()
	if chunk == "" && !finished {
		return
	}
	seq := f.deltaSeq.Add(1)
	var err error
	if f.role == "thinking" {
		_, err = f.emitter.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			return eventlog.PublishAgentThinkingDeltaInTx(ctx, uow, f.cardID, eventlog.AgentThinkingDeltaPayload{
				CardID:   f.cardID,
				RunID:    f.runID,
				DeltaSeq: seq,
				Delta:    chunk,
				Finished: finished,
			}, eventlog.WithActor(f.actor))
		})
	} else {
		_, err = f.emitter.log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			return eventlog.PublishAgentStreamDeltaInTx(ctx, uow, f.cardID, eventlog.AgentStreamDeltaPayload{
				CardID:         f.cardID,
				RunID:          f.runID,
				DeltaSeq:       seq,
				Delta:          chunk,
				Role:           f.role,
				Finished:       finished,
				ConversationID: f.convoID,
			}, eventlog.WithActor(f.actor))
		})
	}
	if err != nil {
		slog.Error("agent: delta flush failed",
			"card_id", f.cardID, "run_id", f.runID, "delta_seq", seq, "err", err)
	}
}
