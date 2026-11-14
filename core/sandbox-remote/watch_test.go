//go:build unix

// Sessions run on the unix spawn path. The Windows backend has its own
// conformance file, and these bodies are POSIX scripts: making this suite run
// there means rendering the bodies (not just the shell) through
// sandboxtest.ForPlatform, which lands with the sessionkit port.

package remote_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
)

// TestWatchStreamsOutputAndExit covers the push path end to end: SSE frames from
// the server, decoded by the client, delivered in order.
func TestWatchStreamsOutputAndExit(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "echo first-line; sleep 0.2; echo second-line"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher, err := sess.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = watcher.Close() }()

	var (
		output   strings.Builder
		sawExit  bool
		lastSeq  int64 = -1
		deadline       = time.After(10 * time.Second)
	)
	for !sawExit {
		select {
		case ev, open := <-watcher.Events():
			if !open {
				t.Fatal("event stream closed before the exit event")
			}
			if ev.Seq < lastSeq {
				t.Fatalf("event seq went backwards: %d after %d", ev.Seq, lastSeq)
			}
			lastSeq = ev.Seq
			switch ev.Type {
			case sandbox.SessionEventOutput:
				output.Write(ev.Data)
			case sandbox.SessionEventExited:
				sawExit = true
			case sandbox.SessionEventLag:
				t.Fatalf("the stream lagged at seq %d; the test consumer never fell behind", ev.Seq)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the exit event")
		}
	}

	if got := output.String(); !strings.Contains(got, "first-line") || !strings.Contains(got, "second-line") {
		t.Fatalf("streamed output = %q, want both lines in order", got)
	}
}

// A session that already finished still replays: the subscription starts from the
// retained log, which is what makes Watch usable after a reconnect.
//
// The replay ends at the exit event, not at a closed channel. A session that has
// merely exited is still open — its output stays readable and the process can be
// closed or reaped — so only Close ends the subscription.
func TestWatchReplaysAnExitedSession(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "echo already-done"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Let the process finish (its output is in the retained log) before anyone
	// subscribes.
	drainToEOF(t, sess)
	if _, err := sess.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher, err := sess.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = watcher.Close() }()

	var output strings.Builder
	replayed := 0
	for {
		select {
		case ev, open := <-watcher.Events():
			if !open {
				t.Fatalf("replay ended without an exit event (replayed %d output events, %q)", replayed, output.String())
			}
			switch ev.Type {
			case sandbox.SessionEventOutput:
				output.Write(ev.Data)
				replayed++
			case sandbox.SessionEventExited:
				if !strings.Contains(output.String(), "already-done") {
					t.Fatalf("replayed output = %q, want the retained line", output.String())
				}
				// Now close, which is what turns a finished session into a
				// finished subscription.
				if err := sess.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				waitClosed(t, watcher.Events())
				return
			}
		case <-ctx.Done():
			t.Fatalf("subscription on an exited session never reported the exit; got %q", output.String())
		}
	}
}

// waitClosed asserts the contract's ending: the channel closes after the Closed
// event, promptly.
func waitClosed(t *testing.T, events <-chan sandbox.SessionEvent) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, open := <-events:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("event stream did not close after the session was closed")
		}
	}
}

// Closing the session ends the subscription: the contract's Closed event is the
// last one and the channel closes right after it.
func TestWatchClosesWithTheSession(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "echo hi; sleep 5"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher, err := sess.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, open := <-watcher.Events():
			if !open {
				return // closed after the Closed event, as promised
			}
			if ev.Type == sandbox.SessionEventClosed {
				// The next receive must report the channel closed; the loop
				// checks that on its own iteration.
				continue
			}
		case <-deadline:
			t.Fatal("subscription outlived the session")
		}
	}
}
