//go:build unix

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestLocalSessionCloseInputIgnoresAlreadyClosed covers the race where
// cmd.Wait (running in reap) closes the parent-side stdin write end just
// before Exec calls CloseInput: closing an already-closed pipe must be a
// no-op success, not an error that fails the whole Exec.
func TestLocalSessionCloseInputIgnoresAlreadyClosed(t *testing.T) {
	_, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	s := &localSession{stdin: w}
	if err := s.CloseInput(); err != nil {
		t.Fatalf("CloseInput on an already-closed stdin = %v, want nil", err)
	}
	if s.stdin != nil {
		t.Fatal("CloseInput should detach the closed stdin")
	}
}

// TestWatcherCloseIsIdempotent verifies the SessionWatcher contract:
// Close may be called more than once, and the events channel still
// terminates (the subscription is over).
func TestWatcherCloseIsIdempotent(t *testing.T) {
	l := newOutputLog(1 << 20)
	w := l.subscribe()
	go w.run(context.Background())

	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	select {
	case _, ok := <-w.ch:
		if ok {
			t.Fatal("events channel should be closed after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("events channel did not close after Close")
	}
}

// TestWatcherConcurrentClose covers the close-of-closed-channel race on
// the watcher's internal stop channel: a consumer calling Close at the
// same time the owning session tears down through outputLog.close must
// not double-close. The race detector flags the old unsynchronized
// check-then-close pattern even when the panic does not fire.
func TestWatcherConcurrentClose(t *testing.T) {
	for i := 0; i < 200; i++ {
		l := newOutputLog(1 << 20)
		w := l.subscribe()
		go w.run(context.Background())

		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); _ = w.Close() }()
		go func() { defer wg.Done(); _ = w.Close() }()
		go func() { defer wg.Done(); l.close() }()
		wg.Wait()

		// outputLog.close delivered the Closed event and closed the
		// channel, so draining must terminate promptly.
		for range w.ch {
		}
	}
}

// TestInterruptEndsAChildSpawnedFromABlockedMask pins the spawn-side
// contract behind Session.Signal(Interrupt): a child must not inherit the
// forking thread's signal mask.
//
// A mask survives exec while dispositions do not, so a child that starts
// with SIGINT blocked can never be interrupted - kill(2) to its group
// succeeds, the signal stays pending, and the process runs to its own
// end, which is how an interrupt turns into a silent no-op. Go reuses
// threads that entered the runtime from C, and those keep the mask their
// thread arrived with, so this blocks SIGINT on its own thread and spawns
// the session from it: with the block cleared for the fork, the interrupt
// ends the child like any other.
func TestInterruptEndsAChildSpawnedFromABlockedMask(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	previous, err := replaceThreadSignalMask(sigsetOnly(sigintNumber))
	if err != nil {
		t.Skipf("signal masks are not available here: %v", err)
	}
	defer func() {
		if _, err := replaceThreadSignalMask(previous); err != nil {
			t.Errorf("restore thread signal mask: %v", err)
		}
	}()

	cmd := exec.Command("/bin/sleep", "30")
	sess, err := StartSession(context.Background(), SessionSpec{ID: "dirty-mask"}, cmd)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Signal(context.Background(), SessionSignalInterrupt); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sess.Wait(waitCtx); err != nil {
		t.Fatalf("wait after interrupt: %v", err)
	}
}
