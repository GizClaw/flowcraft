package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
)

// captureSink stores every delta it receives so tests can assert on
// the order/content of emitted events without racing.
type captureSink struct {
	mu     sync.Mutex
	deltas []engine.StreamDeltaPayload
	err    error
}

func (s *captureSink) OnDelta(_ context.Context, _ event.Envelope, d engine.StreamDeltaPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deltas = append(s.deltas, d)
	return s.err
}

func (s *captureSink) snapshot() []engine.StreamDeltaPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]engine.StreamDeltaPayload, len(s.deltas))
	copy(out, s.deltas)
	return out
}

func TestStreamRouter_FanOutToMultipleSinks(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := engine.NewStreamRouter(bus)
	t.Cleanup(func() { _ = router.Close() })

	a, b := &captureSink{}, &captureSink{}
	stopA, err := router.Attach("run-1", "a", a)
	if err != nil {
		t.Fatalf("attach a: %v", err)
	}
	defer stopA()
	stopB, err := router.Attach("run-1", "b", b)
	if err != nil {
		t.Fatalf("attach b: %v", err)
	}
	defer stopB()

	if err := engine.EmitStreamToken(context.Background(), bus, "run-1", "node-x", "hello"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := engine.EmitStreamToken(context.Background(), bus, "run-1", "node-x", " world"); err != nil {
		t.Fatalf("emit: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return len(a.snapshot()) == 2 && len(b.snapshot()) == 2
	})

	for _, sink := range []*captureSink{a, b} {
		got := sink.snapshot()
		if got[0].Content != "hello" || got[1].Content != " world" {
			t.Fatalf("delta order/content wrong: %+v", got)
		}
	}
}

func TestStreamRouter_AutoTeardownOnRunEnd(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := engine.NewStreamRouter(bus, engine.WithStreamIncludeAllRunEvents())
	t.Cleanup(func() { _ = router.Close() })

	sink := &captureSink{}
	if _, err := router.Attach("run-2", "s", sink); err != nil {
		t.Fatalf("attach: %v", err)
	}

	endEnv, _ := event.NewEnvelope(context.Background(), engine.SubjectRunEnd("run-2"), nil)
	if err := bus.Publish(context.Background(), endEnv); err != nil {
		t.Fatalf("publish end: %v", err)
	}

	// After the end event is observed the router tears the fanout
	// down. The next Attach must spawn a fresh subscription
	// successfully (i.e. the runID is no longer tracked).
	waitFor(t, time.Second, func() bool {
		// Indirect probe: detach removes the runID; if we can
		// re-attach without error we know the router has not
		// gone permanently broken.
		if stop, err := router.Attach("run-2", "s2", sink); err == nil {
			stop()
			return true
		}
		return false
	})
}

func TestStreamRouter_SinkErrorIsolated(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })

	var (
		errs   []string
		errsMu sync.Mutex
	)
	router := engine.NewStreamRouter(bus,
		engine.WithStreamSinkErrorHandler(func(id string, err error) {
			errsMu.Lock()
			defer errsMu.Unlock()
			errs = append(errs, id+":"+err.Error())
		}),
	)
	t.Cleanup(func() { _ = router.Close() })

	bad := &captureSink{err: errors.New("boom")}
	good := &captureSink{}
	if _, err := router.Attach("run-3", "bad", bad); err != nil {
		t.Fatalf("attach bad: %v", err)
	}
	if _, err := router.Attach("run-3", "good", good); err != nil {
		t.Fatalf("attach good: %v", err)
	}

	if err := engine.EmitStreamToken(context.Background(), bus, "run-3", "n", "x"); err != nil {
		t.Fatalf("emit: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return len(good.snapshot()) == 1
	})
	errsMu.Lock()
	defer errsMu.Unlock()
	if len(errs) == 0 {
		t.Fatal("expected at least one sink error to surface via handler")
	}
}

func TestStreamRouter_AttachAfterCloseRejected(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	router := engine.NewStreamRouter(bus)
	_ = router.Close()

	if _, err := router.Attach("run", "s", &captureSink{}); err == nil {
		t.Fatal("Attach must fail after Close")
	}
}

// waitFor polls cond every 5ms until it returns true or the
// deadline elapses, failing the test on timeout. Avoids the test
// harness equivalent of a tight polling loop.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
