//go:build unix

package sessionkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// The reaping layer's failure modes: a child that kills itself, a Terminate that
// races the child's own exit, and a handle that is closed while still readable.

func TestSessionKilledBySignal(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{}, "kill -9 $$")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Reason != sandbox.SessionSignaled {
		t.Fatalf("reason = %v, want signaled (exit %+v)", exit.Reason, exit)
	}
	if exit.Signal != 9 {
		t.Fatalf("signal = %d, want 9", exit.Signal)
	}

	// The log is still readable after a signal: the contract says so, and it is
	// the only way a caller sees what the child printed before dying.
	if _, err := sess.Read(ctx, 0, 64); err != nil {
		t.Fatalf("Read after a signal: %v", err)
	}
}

// Terminate immediately after Start races the child's own exit. Neither side may
// panic, and Wait has to settle on a verdict either way.
func TestTerminateImmediatelyAfterStart(t *testing.T) {
	for i := range 20 {
		sess := startSh(t, sandbox.SessionSpec{}, "true")

		// Ignore the error: the child may have exited between the two calls, and
		// Terminate is documented as idempotent on an exited process.
		_ = sess.Terminate(context.Background())

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		exit, err := sess.Wait(ctx)
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: Wait: %v", i, err)
		}
		switch exit.Reason {
		case sandbox.SessionExited, sandbox.SessionTerminated, sandbox.SessionSignaled:
		default:
			t.Fatalf("iteration %d: reason = %v (exit %+v)", i, exit.Reason, exit)
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
	}
}

// Wait is cached: repeating it after Close returns the exit the reaper already
// observed instead of failing on a released handle.
func TestWaitSurvivesClose(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{}, "exit 7")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if first.Code != 7 {
		t.Fatalf("exit = %+v, want code 7", first)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait after Close: %v", err)
	}
	if second != first {
		t.Fatalf("Wait after Close = %+v, want the cached %+v", second, first)
	}

	// Everything but Wait refuses on a closed handle.
	if _, err := sess.Read(ctx, 0, 16); !errors.Is(err, sandbox.ErrSessionClosed) {
		t.Errorf("Read after Close = %v, want ErrSessionClosed", err)
	}
	if err := sess.Write(ctx, []byte("x")); !errors.Is(err, sandbox.ErrSessionClosed) {
		t.Errorf("Write after Close = %v, want ErrSessionClosed", err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// A session whose output is still being copied when it is closed must not panic
// or leak: Close waits for the copier before releasing the log.
func TestCloseRacingOutput(t *testing.T) {
	for i := range 20 {
		sess := startSh(t, sandbox.SessionSpec{}, "i=0; while [ $i -lt 200 ]; do echo line-$i; i=$((i+1)); done")
		if err := sess.Close(); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
		if _, err := sess.Wait(context.Background()); err != nil {
			t.Fatalf("iteration %d: Wait: %v", i, err)
		}
	}
}
