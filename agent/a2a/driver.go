package a2a

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/agent/a2a/wire"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/goccy/go-json"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// section is the checkpoint section this driver owns inside the task the host minted.
//
// It is a section rather than its own task row because the task is the host's: it names the
// session, it is what a resume looks up, and the decorator writes its terminal state. Sections
// merge across saves, so a section written here survives that write, and the peer's identity is
// read back from the same place it was put.
const section = "a2a"

// spanSend is one round trip with the peer. It is the only span this side can open: the model call
// and the tool calls happen on the peer, where its own trace records them, and this span is what
// links this run's trace to that one — the trace context is injected into the request headers, so
// the peer's spans hang under this one when it honours the propagation.
const spanSend = "a2a.send"

// remote is the peer's identity for one session, as the store remembers it.
type remote struct {
	// TaskID is the peer's task to continue, which is not the local task id: the peer names its
	// own. It is cleared when the task finishes, so the next message starts a new one.
	TaskID string `json:"task_id,omitempty"`
	// LastTaskID is the last task the peer ran for this session, finished or not. It survives a
	// terminal state because the context does: a restart continues the conversation even though
	// the task it belonged to is over.
	LastTaskID string `json:"last_task_id,omitempty"`
	// ContextID outlives the task: the next message continues the session even when the task
	// it belonged to finished.
	ContextID string `json:"context_id,omitempty"`
	// State is what the peer last reported, so a restart reports the same lifecycle.
	State agent.TaskState `json:"state,omitempty"`
}

// driver is one session's conversation with the peer.
//
// It owns the run-scoped verbs without a key, because the host refuses a second turn per session:
// the instance is the key, which is what lets [driver.Cancel] mean "this conversation's task".
type driver struct {
	client  *a2aclient.Client
	session string

	mu sync.Mutex
	// remote is the peer's task and context; restored once, from the store.
	remote   remote
	restored bool
	// task is the local task and store this turn was given, kept so Cancel has an address
	// without repeating the request.
	task  agent.Task
	store agent.Store
	// round counts the lifecycle transitions the peer reported for its current task: it is the
	// projection's answer to "how far did this get" for a task nobody on this side steps through.
	round int
}

var (
	_ agent.Driver    = (*driver)(nil)
	_ agent.Canceller = (*driver)(nil)
	_ agent.Rounder   = (*driver)(nil)
)

// Round reports how far the peer's task has got, as the number of lifecycle transitions it
// reported. The turn decorator writes it into the projection because a proxy has no other answer.
func (d *driver) Round() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.round
}

// Run forwards one message and streams the peer's events back as core events.
//
// The peer decides whether the message starts a task or answers one: an input-required task is
// carried in the task id, not in a separate call, so the driver sends the same request either way.
func (d *driver) Run(ctx context.Context, req agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		d.remember(req)
		d.restore(ctx)

		outgoing, err := wire.Message(req.Input)
		if err != nil {
			yield(nil, err)
			return
		}
		d.mu.Lock()
		outgoing.TaskID = a2a.TaskID(d.remote.TaskID)
		outgoing.ContextID = d.remote.ContextID
		d.remote.State = agent.StateWorking
		peerTask, peerContext := d.remote.TaskID, d.remote.ContextID
		d.mu.Unlock()

		// One span per round trip: it is the boundary only this side can see, and the trace
		// context injected into the request headers is what makes the peer's own spans its
		// children when the peer honours the propagation.
		ctx, span := telemetry.Start(ctx, telemetry.TracerWithSuffix("a2a"), spanSend,
			telemetry.TraceCall,
			trace.WithAttributes(
				attribute.String(telemetry.AttrA2APeerTaskID, peerTask),
				attribute.String(telemetry.AttrA2AContextID, peerContext),
			))
		span.SetAttributes(telemetry.Attrs(ctx)...)
		started := time.Now()
		// failure is what the round trip ended on, so the deferred recorder, the span and the
		// record agree about the outcome instead of each re-reading the driver's state.
		var failure error

		// The peer's identity must outlive this call however the stream ends, and so must the
		// record of the call: both are written on a context that ignores the cancellation that
		// stopped the stream.
		defer func() {
			d.persist(ctx, req)
			d.recordCall(ctx, req, started, failure)
			finishSendSpan(span, d.State(), failure)
			span.End()
		}()

		if !yield(agent.StreamStart{Model: "a2a"}, nil) {
			return
		}
		for event, streamErr := range d.client.SendStreamingMessage(ctx, &a2a.SendMessageRequest{
			Message: outgoing,
		}) {
			if streamErr != nil {
				failure = streamErr
				if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
					d.setState(agent.StateCanceled)
					yield(agent.Status{State: agent.StateCanceled, Reason: streamErr.Error()}, nil)
					return
				}
				d.setState(agent.StateFailed)
				yield(agent.Status{State: agent.StateFailed, Reason: streamErr.Error()}, nil)
				// The cause must reach the caller: a status alone would make a protocol failure
				// indistinguishable from a task the peer refused.
				yield(nil, streamErr)
				return
			}
			done, handleErr := d.handle(event, yield)
			if handleErr != nil {
				failure = handleErr
				yield(nil, handleErr)
				return
			}
			if done {
				return
			}
		}
		// A stream that ends while still working completed without saying so; a paused stream
		// ended exactly where it meant to, and overwriting that with "completed" would lose the
		// question the peer asked.
		switch d.State() {
		case agent.StateSubmitted, agent.StateWorking:
			d.setState(agent.StateCompleted)
			yield(agent.Status{State: agent.StateCompleted}, nil)
		}
	}
}

// remember keeps the turn's address, so a cancellation can name the peer's task without the
// caller repeating the request.
func (d *driver) remember(req agent.Request) {
	d.mu.Lock()
	d.task = req.Task
	d.store = req.Store
	d.mu.Unlock()
}

// restore reads the peer's identity from the task's section, once per driver.
//
// It is lazy because [Program.Open] has no store: the store is a property of the turn, not of the
// conversation, and a driver opened before the first turn has nothing to read it with.
func (d *driver) restore(ctx context.Context) {
	d.mu.Lock()
	done := d.restored
	store := d.store
	taskID := d.task.ID
	d.restored = true
	d.mu.Unlock()
	if done || store == nil || taskID == "" {
		return
	}
	checkpoint, err := store.Load(ctx, taskID)
	if err != nil {
		// Nothing stored is a first turn, not a failure: the peer mints the task.
		return
	}
	raw, ok := checkpoint.Sections[section]
	if !ok {
		return
	}
	var remembered remote
	if err := json.Unmarshal(raw, &remembered); err != nil {
		telemetry.WarnErr(ctx, "a2a: decode the peer's identity", err)
		return
	}
	if remembered.State.Terminal() {
		// A finished task is not resumed — the peer would refuse a message for it — but the
		// context is what carries over, so it stays.
		remembered.TaskID = ""
	}
	d.mu.Lock()
	d.remote = remembered
	d.mu.Unlock()
}

// persist records the peer's identity in the task's section so a restart can continue it.
//
// Best effort: a proxy whose identity could not be saved still answered the caller, and failing
// the answer over it would be worse than losing a resume point.
func (d *driver) persist(ctx context.Context, req agent.Request) {
	d.mu.Lock()
	remembered := d.remote
	store := d.store
	task := d.task
	d.mu.Unlock()
	if store == nil || task.ID == "" {
		return
	}
	raw, err := json.Marshal(remembered)
	if err != nil {
		telemetry.WarnErr(ctx, "a2a: encode the peer's identity", err)
		return
	}
	_, _, err = store.Save(context.WithoutCancel(ctx), agent.Checkpoint{
		Schema:      agent.Schema,
		Fingerprint: req.Fingerprint,
		Task: agent.Task{
			ID:        task.ID,
			SessionID: d.session,
			State:     remembered.State,
		},
		Sections: map[string]json.RawMessage{section: raw},
	}, nil)
	if err != nil {
		telemetry.WarnErr(ctx, "a2a: persist the remote task", err)
	}
}

// State reports the lifecycle the peer last reported.
func (d *driver) State() agent.TaskState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.remote.State
}

func (d *driver) setState(state agent.TaskState) {
	d.mu.Lock()
	if state != d.remote.State {
		d.round++
	}
	d.remote.State = state
	d.mu.Unlock()
}

// Cancel asks the peer to stop the task.
//
// A local driver is cancelled through the context; this one must also tell the peer, because
// dropping the context leaves the remote run going. Nothing to stop — no task yet, or one the peer
// already finished — is a nil, not an error: the caller's cancellation is satisfied either way.
func (d *driver) Cancel(ctx context.Context, reason string) error {
	d.mu.Lock()
	taskID := d.remote.TaskID
	state := d.remote.State
	d.mu.Unlock()
	if taskID == "" || state.Terminal() {
		return nil
	}
	if _, err := d.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(taskID)}); err != nil {
		return err
	}
	d.setState(agent.StateCanceled)
	return nil
}

// Close releases the driver. The peer's identity lives in the store, so a closed driver can be
// opened again and resume from the section it wrote.
func (d *driver) Close() error {
	d.mu.Lock()
	d.restored = false
	d.mu.Unlock()
	return nil
}
