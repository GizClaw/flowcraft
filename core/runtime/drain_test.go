package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

func TestRuntimeDrainQuiescesAndWaitsForActiveTurn(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	app, _ := buildLifecycleApp(t, lifecycleDoc(t), blockingRunEngine(release))

	lease, err := app.Sessions().Open(context.Background(), session.Key{
		AgentID: "bot", ContextID: "ctx",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	drained := make(chan error, 1)
	go func() {
		drained <- app.Drain(context.Background())
	}()

	// Wait until the manager has entered its draining state so the
	// quiesce assertions cannot race the goroutine above. Opens succeed
	// before drain begins and are closed again immediately.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("Runtime.Drain did not start draining")
		}
		probe, err := app.Sessions().Open(context.Background(), session.Key{
			AgentID: "bot", ContextID: "probe",
		})
		if err != nil {
			if !errors.Is(err, session.ErrManagerDraining) {
				t.Fatalf("Open while waiting for drain error = %v, want ErrManagerDraining", err)
			}
			break
		}
		_ = probe.Close()
		time.Sleep(time.Millisecond)
	}

	if _, err := app.Sessions().Open(context.Background(), session.Key{
		AgentID: "bot", ContextID: "quiesced",
	}); !errors.Is(err, session.ErrManagerDraining) {
		t.Fatalf("Open during runtime drain error = %v, want ErrManagerDraining", err)
	}
	if _, err := lease.Session().Start(context.Background(), agent.Request{}); !errors.Is(err, session.ErrManagerDraining) {
		t.Fatalf("Start during runtime drain error = %v, want ErrManagerDraining", err)
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Runtime.Drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Runtime.Drain did not return after turn finished")
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("turn Wait: %v", err)
	}
	if !app.manager.Idle() {
		t.Fatal("runtime manager not idle after Runtime.Drain returned")
	}

	// The old runtime stays quiesced until Close; Close must still work
	// after a successful drain.
	if err := app.Drain(context.Background()); err != nil {
		t.Fatalf("second Runtime.Drain: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close after Drain: %v", err)
	}
	if err := app.Drain(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Runtime.Drain after Close error = %v, want not available", err)
	}
}
