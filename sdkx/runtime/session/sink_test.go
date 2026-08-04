package session

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
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
