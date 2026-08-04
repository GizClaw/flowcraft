package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
)

func TestQueuedSinkDetachEnqueueRaceCallsOnDetachOnce(t *testing.T) {
	session := &Session{}
	session.changeActivity(activitySink, 1)
	var detachCalls atomic.Int64
	sink := newQueuedSink(session, "run", SinkSpec{
		ID:        "race",
		Sink:      agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error { return nil }),
		OnDetach:  func(error) { detachCalls.Add(1) },
		QueueSize: 4,
	}, 4)
	sink.start()

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 64 {
				_ = sink.OnDelta(context.Background(), event.Envelope{}, agent.StreamDeltaPayload{})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sink.detach(nil)
	}()
	wg.Wait()
	eventually(t, time.Second, func() bool { return detachCalls.Load() == 1 })
	session.mu.Lock()
	got := session.attachedSinks
	session.mu.Unlock()
	if got != 0 {
		t.Fatalf("attached sinks = %d, want 0", got)
	}
}

func TestQueuedSinkStoppedDeliveryDoesNotAdvanceAuthoritativeAck(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	turn := newTurn(nil, "run", context.Background())
	sink := newQueuedSink(nil, "run", SinkSpec{
		ID: "authority",
		Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
			close(entered)
			<-release
			return nil
		}),
		Visibility: VisibilityConfirmed,
		Authority:  AuthorityAuthoritative,
	}, 1)
	sink.offered = turn.sinkOffered
	sink.delivered = turn.sinkDelivered
	sink.onDetach = func(err error) { turn.sinkDetached("authority", err) }
	turn.configureAuthority(sink.spec, 1, sink)
	sink.start()
	env := event.Envelope{Headers: map[string]string{HeaderDeliveryCursor: "1"}}
	_ = sink.OnDelta(context.Background(), env, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaToken, Content: "not-delivered",
	})
	<-entered
	sink.detach(context.Canceled)
	close(release)
	eventually(t, time.Second, func() bool {
		turn.mu.Lock()
		defer turn.mu.Unlock()
		return turn.frozen
	})
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.deliveredCursor != 0 || turn.ackedCursor != 0 || turn.frozenPrefix != "" {
		t.Fatalf("delivery advanced after stop: delivered=%d acked=%d prefix=%q",
			turn.deliveredCursor, turn.ackedCursor, turn.frozenPrefix)
	}
}

func TestAckOnDeliveryLinearizesAfterSinkCallbackReturns(t *testing.T) {
	turn := newTurn(nil, "run", context.Background())
	sink := newQueuedSink(nil, "run", SinkSpec{
		ID: "authority",
		Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
			return turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput})
		}),
		Visibility: VisibilityConfirmed,
		Authority:  AuthorityAuthoritative,
	}, 1)
	sink.delivered = turn.sinkDelivered
	sink.onDetach = func(err error) { turn.sinkDetached("authority", err) }
	turn.configureAuthority(sink.spec, 1, sink)
	turn.recordConfirmed(1, 1, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaToken, Content: "not-acknowledged",
	})
	sink.start()
	_ = sink.OnDelta(
		context.Background(),
		event.Envelope{Headers: map[string]string{HeaderDeliveryCursor: "1"}},
		agent.StreamDeltaPayload{Type: agent.StreamDeltaToken, Content: "not-acknowledged"},
	)

	eventually(t, time.Second, func() bool {
		turn.mu.Lock()
		defer turn.mu.Unlock()
		return turn.deliveredCursor == 1
	})
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.ackedCursor != 0 || turn.frozenCursor != 0 || turn.frozenPrefix != "" {
		t.Fatalf("callback-in-flight delivery entered frozen prefix: acked=%d frozen=%d prefix=%q",
			turn.ackedCursor, turn.frozenCursor, turn.frozenPrefix)
	}
}

func TestAckExplicitAllowsCallbackAckOnlyThroughOfferedCursor(t *testing.T) {
	turn := newTurn(nil, "run", context.Background())
	callbackErr := errors.New("sink failed after consuming delivery")
	futureAck := make(chan error, 1)
	sink := newQueuedSink(nil, "run", SinkSpec{
		ID: "authority",
		Sink: agent.StreamSinkFunc(func(
			_ context.Context,
			env event.Envelope,
			_ agent.StreamDeltaPayload,
		) error {
			cursor, err := DeliveryCursorFromEnvelope(env)
			if err != nil {
				return err
			}
			if err := turn.Ack("authority", cursor); err != nil {
				return err
			}
			futureAck <- turn.Ack("authority", cursor+1)
			if err := turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput}); err != nil {
				return err
			}
			return callbackErr
		}),
		Visibility: VisibilityConfirmed,
		Authority:  AuthorityAuthoritative,
		AckMode:    AckExplicit,
	}, 1)
	sink.offered = turn.sinkOffered
	sink.delivered = turn.sinkDelivered
	sink.onDetach = func(err error) { turn.sinkDetached("authority", err) }
	turn.configureAuthority(sink.spec, 1, sink)
	turn.recordConfirmed(1, 1, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaToken, Content: "consumed",
	})
	sink.start()
	_ = sink.OnDelta(
		context.Background(),
		event.Envelope{Headers: map[string]string{HeaderDeliveryCursor: "1"}},
		agent.StreamDeltaPayload{Type: agent.StreamDeltaToken, Content: "consumed"},
	)

	if err := <-futureAck; !errdefs.IsConflict(err) {
		t.Fatalf("future ACK error = %v, want conflict", err)
	}
	sink.wait()
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.ackedCursor != 1 || turn.frozenCursor != 1 || turn.frozenPrefix != "consumed" {
		t.Fatalf("explicit ACK not retained after callback error: acked=%d frozen=%d prefix=%q",
			turn.ackedCursor, turn.frozenCursor, turn.frozenPrefix)
	}
}

func TestQueuedSinkInternalAndUserDetachCallbacksExactlyOnce(t *testing.T) {
	var internal, user atomic.Int64
	sink := newQueuedSink(nil, "run", SinkSpec{
		ID: "sink",
		Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
			return nil
		}),
		OnDetach: func(error) { user.Add(1) },
	}, 1)
	sink.onDetach = func(error) { internal.Add(1) }
	sink.detach(nil)
	sink.detach(context.Canceled)
	eventually(t, time.Second, func() bool { return user.Load() == 1 })
	if internal.Load() != 1 {
		t.Fatalf("internal detach calls = %d", internal.Load())
	}
}
