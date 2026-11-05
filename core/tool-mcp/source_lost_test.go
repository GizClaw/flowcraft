package mcp

import (
	"context"
	"io/fs"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestSource_UnrunnableCommandGivesUpAndReportsLoss covers the terminal
// class end to end: a stdio command the OS cannot start must not be
// retried forever, WaitReady must fail with the classification instead
// of waiting out its timeout, and the configured observer must be told
// exactly once.
func TestSource_UnrunnableCommandGivesUpAndReportsLoss(t *testing.T) {
	t.Parallel()
	src := NewSource(WithRetryBackoff(10*time.Millisecond, 20*time.Millisecond))
	t.Cleanup(func() { _ = src.Close() })

	var mu sync.Mutex
	var lost []error
	transport, err := Stdio(filepath.Join(t.TempDir(), "missing-server"), nil, nil)
	if err != nil {
		t.Fatalf("Stdio: %v", err)
	}
	err = src.AddServer(context.Background(), "broken", transport,
		WithServerLost(func(_ string, err error) {
			mu.Lock()
			lost = append(lost, err)
			mu.Unlock()
		}))
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	start := time.Now()
	err = src.WaitReady(context.Background(), "broken", 5*time.Second)
	elapsed := time.Since(start)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("WaitReady = %v after %s, want Validation", err, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("WaitReady waited %s; an unrunnable command must be given up immediately",
			elapsed)
	}
	// Give a loop that refused to give up a chance to prove it.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(lost) != 1 {
		t.Fatalf("loss callbacks = %d, want 1", len(lost))
	}
	if lost[0] == nil {
		t.Fatal("loss callback carried no error")
	}
}

// TestSource_LossIsSilentForTransientReconnects pins the other half of
// the taxonomy: a server whose child exits and is reconnected in place
// must not flap the observer.
func TestSource_LossIsSilentForTransientReconnects(t *testing.T) {
	t.Parallel()
	src := NewSource(
		WithRetryBackoff(30*time.Millisecond, 100*time.Millisecond),
		WithLivenessInterval(50*time.Millisecond),
	)
	t.Cleanup(func() { _ = src.Close() })

	var mu sync.Mutex
	lost := 0
	transport, err := helperStdio(0, 250)
	if err != nil {
		t.Fatalf("helperStdio: %v", err)
	}
	err = src.AddServer(context.Background(), "flapping", transport,
		WithServerLost(func(string, error) {
			mu.Lock()
			lost++
			mu.Unlock()
		}))
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "first connect", func() bool {
		return len(src.Tools()) == 1
	})
	helper := src.Tools()[0]
	execute := func() error {
		_, err := helper.Execute(context.Background(), "{}")
		return err
	}
	waitFor(t, 5*time.Second, "the child's exit to surface", func() bool {
		return errdefs.IsNotAvailable(execute())
	})
	waitFor(t, 10*time.Second, "the reconnect to restore the server", func() bool {
		return execute() == nil
	})
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if lost != 0 {
		t.Fatalf("loss callbacks = %d for a server that came back, want 0", lost)
	}
}

// afterFirstTransport connects to a real stdio helper the first time
// and then fails the way a removed binary does, so that a live server's
// death reaches the "the reconnect cannot succeed" state
// deterministically.
type afterFirstTransport struct {
	inner mcpsdk.Transport
	calls atomic.Int32
}

func (t *afterFirstTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	if t.calls.Add(1) > 1 {
		return nil, &fs.PathError{
			Op: "fork/exec", Path: "server", Err: fs.ErrNotExist,
		}
	}
	return t.inner.Connect(ctx)
}

var _ mcpsdk.Transport = (*afterFirstTransport)(nil)

// TestSource_ReportsLossWhenReconnectCannotSucceed covers the reported
// terminal loss after a live server: the child is gone, the command
// cannot be spawned again, and the observer learns about it once.
func TestSource_ReportsLossWhenReconnectCannotSucceed(t *testing.T) {
	t.Parallel()
	src := NewSource(WithRetryBackoff(30*time.Millisecond, 100*time.Millisecond))
	t.Cleanup(func() { _ = src.Close() })

	lost := make(chan error, 4)
	inner, err := helperStdio(0, 250)
	if err != nil {
		t.Fatalf("helperStdio: %v", err)
	}
	err = src.AddServer(context.Background(), "dying", &afterFirstTransport{inner: inner},
		WithServerLost(func(_ string, err error) { lost <- err }))
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	waitFor(t, 5*time.Second, "first connect", func() bool {
		return len(src.Tools()) == 1
	})
	helper := src.Tools()[0]
	waitFor(t, 5*time.Second, "the child's exit to surface", func() bool {
		_, err := helper.Execute(context.Background(), "{}")
		return errdefs.IsNotAvailable(err)
	})
	select {
	case err := <-lost:
		if err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("loss error = %v, want the terminal Validation failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no loss reported after the reconnect could not spawn the command")
	}
	select {
	case err := <-lost:
		t.Fatalf("a second loss was reported: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}
