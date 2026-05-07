package vesselquality

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// TestSidecar_TriggersOnBusEnvelope asserts a Sidecar agent with
// SubscribeTo runs once per matching envelope. We publish 3
// envelopes onto the vessel bus; the sidecar's engine MUST fire
// exactly 3 times. A non-matching subject is also published to
// confirm the pattern filter works.
func TestSidecar_TriggersOnBusEnvelope(t *testing.T) {
	t.Parallel()
	bus := event.NewMemoryBus()

	var fires int32
	sidecarEngine := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		atomic.AddInt32(&fires, 1)
		return b, nil
	})

	vs := spec.Spec{
		ID: "v-sidecar",
		Agents: []spec.Agent{
			{Name: "watcher", Sidecar: true, SubscribeTo: "audit.*"},
		},
	}
	c := launchedCaptain(t, vs,
		vessel.WithBus(bus),
		vessel.WithEngine(sidecarEngine),
	)
	_ = c // captain anchored by t.Cleanup inside launchedCaptain

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := bus.Publish(ctx, event.Envelope{Subject: "audit.thing"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	// Non-matching subject — the watcher must NOT fire.
	if err := bus.Publish(ctx, event.Envelope{Subject: "other.thing"}); err != nil {
		t.Fatalf("publish nonmatch: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fires) == 3 {
			// Give the bus 100ms to (incorrectly) deliver the
			// non-matching envelope; if filter is correct,
			// fires stays at 3.
			time.Sleep(100 * time.Millisecond)
			if got := atomic.LoadInt32(&fires); got != 3 {
				t.Fatalf("watcher fired %d times after non-matching publish; pattern filter broken", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sidecar fired %d times in 2s, want 3", atomic.LoadInt32(&fires))
}

// TestSidecar_ReceivesEnvelopeViaInputs asserts the runSidecarLoop
// passes the triggering envelope through agent.Request.Inputs
// under the documented "envelope" key, so the sidecar engine can
// inspect the original subject / headers / payload.
func TestSidecar_ReceivesEnvelopeViaInputs(t *testing.T) {
	t.Parallel()
	bus := event.NewMemoryBus()

	type captured struct {
		subject string
		headers map[string]string
	}
	var capCh = make(chan captured, 1)

	sidecarEngine := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		envAny, _ := b.GetVar("envelope")
		env, ok := envAny.(event.Envelope)
		if !ok {
			return b, nil
		}
		select {
		case capCh <- captured{subject: string(env.Subject), headers: env.Headers}:
		default:
		}
		return b, nil
	})

	vs := spec.Spec{
		ID: "v-sidecar-inputs",
		Agents: []spec.Agent{
			{Name: "logger", Sidecar: true, SubscribeTo: "log.>"},
		},
	}
	c := launchedCaptain(t, vs,
		vessel.WithBus(bus),
		vessel.WithEngine(sidecarEngine),
	)
	_ = c

	want := event.Envelope{
		Subject: "log.warn",
		Headers: map[string]string{"trace_id": "abc-123"},
	}
	if err := bus.Publish(context.Background(), want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-capCh:
		if got.subject != "log.warn" {
			t.Errorf("subject = %q, want log.warn", got.subject)
		}
		if got.headers["trace_id"] != "abc-123" {
			t.Errorf("headers[trace_id] = %q, want abc-123", got.headers["trace_id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar engine never received the envelope")
	}
}

// TestSidecar_RejectsManualSubmit asserts the documented contract:
// sidecar agents are bus-triggered only and Submit/Call MUST
// reject them with a clear validation error rather than silently
// running a "manual" turn that would then be racing the bus loop.
func TestSidecar_RejectsManualSubmit(t *testing.T) {
	t.Parallel()
	bus := event.NewMemoryBus()
	noopEngine := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, nil
	})
	vs := spec.Spec{
		ID: "v-sidecar-manual",
		Agents: []spec.Agent{
			{Name: "watcher", Sidecar: true, SubscribeTo: "x.*"},
		},
	}
	c := launchedCaptain(t, vs,
		vessel.WithBus(bus),
		vessel.WithEngine(noopEngine),
	)

	_, err := c.Call(context.Background(), "watcher", agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "ping"),
	})
	if err == nil {
		t.Fatal("Call(sidecar): want error, got nil")
	}
}
