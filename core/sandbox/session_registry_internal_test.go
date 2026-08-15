package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// registryTestSession is a hermetic Session used to exercise
// SessionRegistry bookkeeping without spawning processes.
type registryTestSession struct {
	terminated chan struct{}
	once       sync.Once
}

func (s *registryTestSession) ID() string { return "test-session" }
func (s *registryTestSession) PID() int   { return 42 }

func (s *registryTestSession) Read(context.Context, int64, int) (SessionOutput, error) {
	return SessionOutput{}, nil
}

func (s *registryTestSession) Write(context.Context, []byte) error { return nil }
func (s *registryTestSession) CloseInput() error                   { return nil }
func (s *registryTestSession) Resize(context.Context, int, int) error {
	return nil
}
func (s *registryTestSession) Signal(context.Context, SessionSignal) error { return nil }
func (s *registryTestSession) Terminate(context.Context) error {
	if s.terminated != nil {
		s.once.Do(func() { close(s.terminated) })
	}
	return nil
}
func (s *registryTestSession) Wait(ctx context.Context) (SessionExit, error) {
	// Mirror a real session: Wait blocks until the process exits, i.e.
	// until Terminate fires. Without this, the registry's background
	// tracker would mark the record exited before Close's Terminate
	// runs, and Terminate would skip the still-"running" session.
	if s.terminated != nil {
		select {
		case <-s.terminated:
		case <-ctx.Done():
			return SessionExit{}, ctx.Err()
		}
	}
	return SessionExit{Reason: SessionExited, Code: 0}, nil
}
func (s *registryTestSession) Watch(context.Context) (SessionWatcher, error) {
	return nil, nil
}
func (s *registryTestSession) Close() error { return nil }
func (s *registryTestSession) Capabilities() SessionCapabilities {
	return SessionCapabilities{}
}

// blockingTerminateSession simulates a Session whose Terminate ignores
// context cancellation — Close must still be bounded by its deadline.
type blockingTerminateSession struct {
	registryTestSession
}

func (s *blockingTerminateSession) Terminate(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestSessionRegistryCloseWaitsForStartingSession verifies that Close
// does not fail with NotAvailable when a session is still spawning:
// it waits for the spawn to settle and then terminates the session.
func TestSessionRegistryCloseWaitsForStartingSession(t *testing.T) {
	spawnStarted := make(chan struct{})
	releaseSpawn := make(chan struct{})
	terminated := make(chan struct{})
	sess := &registryTestSession{terminated: terminated}
	starter := func(ctx context.Context, _ SessionSpec) (Session, error) {
		close(spawnStarted)
		select {
		case <-releaseSpawn:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return sess, nil
	}
	reg := NewSessionRegistry(starter)

	var startErr error
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		_, startErr = reg.Start(context.Background(), SessionSpec{Argv: []string{"x"}})
	}()
	<-spawnStarted // record exists; session not yet assigned

	closeDone := make(chan error, 1)
	go func() { closeDone <- reg.Close() }()

	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the starting session settled: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseSpawn)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wait for the starting session")
	}
	select {
	case <-terminated:
	case <-time.After(5 * time.Second):
		t.Fatal("starting session was not terminated by Close")
	}
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}
	if startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
}

// TestSessionRegistryCloseIgnoresVanishedSpawn verifies that Close
// treats a spawn that fails while Close is waiting as a non-event:
// the record is removed by Start, so there is nothing left to drain.
func TestSessionRegistryCloseIgnoresVanishedSpawn(t *testing.T) {
	spawnStarted := make(chan struct{})
	releaseSpawn := make(chan struct{})
	starter := func(ctx context.Context, _ SessionSpec) (Session, error) {
		close(spawnStarted)
		select {
		case <-releaseSpawn:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return nil, errors.New("spawn failed")
	}
	reg := NewSessionRegistry(starter)

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		_, _ = reg.Start(context.Background(), SessionSpec{Argv: []string{"x"}})
	}()
	<-spawnStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- reg.Close() }()
	close(releaseSpawn)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the spawn failed")
	}
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}
}

// TestSessionRegistryCloseBoundedByDeadline verifies that Close honors
// its overall budget even when a Session's Terminate ignores context
// cancellation and blocks forever.
func TestSessionRegistryCloseBoundedByDeadline(t *testing.T) {
	starter := func(context.Context, SessionSpec) (Session, error) {
		return &blockingTerminateSession{
			// A non-nil terminated channel makes the embedded Wait
			// block like a real running process, so the tracker does
			// not mark the session exited before Close's Terminate.
			registryTestSession: registryTestSession{terminated: make(chan struct{})},
		}, nil
	}
	reg := NewSessionRegistry(starter)
	if _, err := reg.Start(context.Background(), SessionSpec{Argv: []string{"x"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	begin := time.Now()
	err := reg.closeWithTimeout(100 * time.Millisecond)
	elapsed := time.Since(begin)
	if err == nil {
		t.Fatal("Close succeeded despite a Terminate that ignores cancellation; want a deadline error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Close took %v; want it bounded by the close deadline", elapsed)
	}
}
