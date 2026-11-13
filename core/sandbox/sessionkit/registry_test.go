package sessionkit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// fakeSession is a session that exits when told to. It exists so the registry's
// bookkeeping — id uniqueness, List, Terminate, Close — is tested without
// spawning anything.
type fakeSession struct {
	id   string
	pid  int
	caps sandbox.SessionCapabilities

	mu     sync.Mutex
	exit   *sandbox.SessionExit
	closed bool
	done   chan struct{}
}

func newFakeSession(id string) *fakeSession {
	return &fakeSession{id: id, pid: 4242, done: make(chan struct{})}
}

func (f *fakeSession) ID() string                                { return f.id }
func (f *fakeSession) PID() int                                  { return f.pid }
func (f *fakeSession) Capabilities() sandbox.SessionCapabilities { return f.caps }

func (f *fakeSession) Read(context.Context, int64, int) (sandbox.SessionOutput, error) {
	return sandbox.SessionOutput{EOF: true}, nil
}
func (f *fakeSession) Write(context.Context, []byte) error                 { return nil }
func (f *fakeSession) CloseInput() error                                   { return nil }
func (f *fakeSession) Resize(context.Context, int, int) error              { return nil }
func (f *fakeSession) Signal(context.Context, sandbox.SessionSignal) error { return nil }

func (f *fakeSession) Watch(context.Context) (sandbox.SessionWatcher, error) {
	return nil, errdefs.NotAvailablef("fakeSession: no event stream")
}

func (f *fakeSession) Terminate(context.Context) error {
	f.exitWith(sandbox.SessionExit{Code: -1, Reason: sandbox.SessionTerminated})
	return nil
}

func (f *fakeSession) Wait(ctx context.Context) (sandbox.SessionExit, error) {
	select {
	case <-f.done:
		f.mu.Lock()
		defer f.mu.Unlock()
		return *f.exit, nil
	case <-ctx.Done():
		return sandbox.SessionExit{}, ctx.Err()
	}
}

func (f *fakeSession) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return f.Terminate(context.Background())
}

func (f *fakeSession) isExited() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exit != nil
}

func (f *fakeSession) exitWith(exit sandbox.SessionExit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exit != nil {
		return
	}
	f.exit = &exit
	close(f.done)
}

// starter builds a registry whose sessions are fakes, recorded by id.
func starter(sessions map[string]*fakeSession) SessionStarter {
	var mu sync.Mutex
	return func(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
		mu.Lock()
		defer mu.Unlock()
		f := newFakeSession(spec.ID)
		if sessions != nil {
			sessions[spec.ID] = f
		}
		return f, nil
	}
}

func TestRegistryRejectsEmptyArgv(t *testing.T) {
	r := NewSessionRegistry(starter(nil))
	defer func() { _ = r.Close() }()

	if _, err := r.Start(context.Background(), sandbox.SessionSpec{}); !errdefs.IsValidation(err) {
		t.Fatalf("Start with no argv = %v, want Validation", err)
	}
}

func TestRegistryRejectsDuplicateID(t *testing.T) {
	r := NewSessionRegistry(starter(nil))
	defer func() { _ = r.Close() }()

	spec := sandbox.SessionSpec{ID: "dup", Argv: []string{"sh"}}
	if _, err := r.Start(context.Background(), spec); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := r.Start(context.Background(), spec); !errdefs.IsConflict(err) {
		t.Fatalf("second Start = %v, want Conflict", err)
	}
}

// A duplicate id must stay a duplicate even when two Starts race: an id is the
// handle every other operation addresses.
func TestRegistryConcurrentSameID(t *testing.T) {
	r := NewSessionRegistry(starter(nil))
	defer func() { _ = r.Close() }()

	const attempts = 8
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		ok     int
		others []error
	)
	start := make(chan struct{})
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := r.Start(context.Background(), sandbox.SessionSpec{ID: "same", Argv: []string{"sh"}})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				others = append(others, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok != 1 {
		t.Fatalf("%d Starts succeeded with the same id, want 1", ok)
	}
	for _, err := range others {
		if !errdefs.IsConflict(err) {
			t.Fatalf("loser error = %v, want Conflict", err)
		}
	}
}

func TestRegistryListOrderingAndExitState(t *testing.T) {
	sessions := make(map[string]*fakeSession)
	r := NewSessionRegistry(starter(sessions))
	defer func() { _ = r.Close() }()

	for _, id := range []string{"a", "b", "c"} {
		if _, err := r.Start(context.Background(), sandbox.SessionSpec{ID: id, Argv: []string{"sh"}}); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
	}

	first, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("List returned %d entries", len(first))
	}
	for i, want := range []string{"a", "b", "c"} {
		if first[i].ID != want {
			t.Fatalf("List[%d] = %q, want %q (start order)", i, first[i].ID, want)
		}
		if !first[i].Running || first[i].Exit != nil {
			t.Fatalf("List[%d] = %+v, want running with no exit", i, first[i])
		}
		if first[i].PID != 4242 {
			t.Errorf("List[%d].PID = %d", i, first[i].PID)
		}
	}

	// A session that exits on its own shows its exit and stops being running.
	sessions["b"].exitWith(sandbox.SessionExit{Code: 3, Reason: sandbox.SessionExited})
	waitFor(t, "b is reported as exited", func() bool {
		list, _ := r.List(context.Background())
		for _, info := range list {
			if info.ID == "b" {
				return !info.Running && info.Exit != nil && info.Exit.Code == 3
			}
		}
		return false
	})

	// List is stable: the same order twice.
	second, _ := r.List(context.Background())
	if second[0].ID != first[0].ID || second[2].ID != first[2].ID {
		t.Fatalf("List order changed: %v", []string{second[0].ID, second[1].ID, second[2].ID})
	}
}

func TestRegistryTerminateUnknownID(t *testing.T) {
	r := NewSessionRegistry(starter(nil))
	defer func() { _ = r.Close() }()

	if err := r.Terminate(context.Background(), "nope"); !errdefs.IsNotFound(err) {
		t.Fatalf("Terminate(unknown) = %v, want NotFound", err)
	}
}

// Terminate is the management path: List reflects it as soon as it returns, and
// a second call is a no-op because the process already exited.
func TestRegistryTerminateSyncsList(t *testing.T) {
	r := NewSessionRegistry(starter(nil))
	defer func() { _ = r.Close() }()

	sess, err := r.Start(context.Background(), sandbox.SessionSpec{ID: "t", Argv: []string{"sh"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Terminate(context.Background(), sess.ID()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	list, _ := r.List(context.Background())
	if len(list) != 1 || list[0].Running {
		t.Fatalf("List after Terminate = %+v, want one entry, not running", list)
	}
	if list[0].Exit == nil || list[0].Exit.Reason != sandbox.SessionTerminated {
		t.Fatalf("exit = %+v, want terminated", list[0].Exit)
	}
	if err := r.Terminate(context.Background(), sess.ID()); err != nil {
		t.Fatalf("second Terminate = %v, want nil", err)
	}
}

func TestRegistryCloseDrainsSessions(t *testing.T) {
	sessions := make(map[string]*fakeSession)
	r := NewSessionRegistry(starter(sessions))

	for _, id := range []string{"x", "y"} {
		if _, err := r.Start(context.Background(), sandbox.SessionSpec{ID: id, Argv: []string{"sh"}}); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for id, sess := range sessions {
		if !sess.isExited() {
			t.Errorf("session %q was abandoned instead of terminated", id)
		}
	}
	// Records are kept on purpose: List still reports what ran, and none of it
	// is running.
	list, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(sessions) {
		t.Fatalf("List after Close = %d entries, want %d (records are kept)", len(list), len(sessions))
	}
	for _, info := range list {
		if info.Running {
			t.Errorf("session %q is still reported as running after Close", info.ID)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// List is a snapshot. Handing a caller a pointer into the record would let a
// later exit write race with its read, and would let one caller mutate what the
// next List reports.
func TestRegistryListSnapshotsExit(t *testing.T) {
	sessions := make(map[string]*fakeSession)
	r := NewSessionRegistry(starter(sessions))
	defer func() { _ = r.Close() }()

	if _, err := r.Start(context.Background(), sandbox.SessionSpec{ID: "snap", Argv: []string{"sh"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sessions["snap"].exitWith(sandbox.SessionExit{Code: 1, Reason: sandbox.SessionExited})
	waitFor(t, "the exit to be observed", func() bool {
		list, _ := r.List(context.Background())
		return len(list) == 1 && !list[0].Running
	})

	first, _ := r.List(context.Background())
	second, _ := r.List(context.Background())
	if first[0].Exit == nil || second[0].Exit == nil {
		t.Fatal("a finished session has no exit in List")
	}
	if first[0].Exit == second[0].Exit {
		t.Fatal("two List calls returned the same exit pointer; List is not a snapshot")
	}
	*first[0].Exit = sandbox.SessionExit{Code: 99}
	if second[0].Exit.Code != 1 {
		t.Fatal("mutating one snapshot changed another")
	}
}

// Closing the handle (not the runner) is what removes a session from List.
func TestRegistrySessionCloseRemovesIt(t *testing.T) {
	r := NewSessionRegistry(starter(nil))
	defer func() { _ = r.Close() }()

	sess, err := r.Start(context.Background(), sandbox.SessionSpec{ID: "h", Argv: []string{"sh"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("session Close: %v", err)
	}
	if list, _ := r.List(context.Background()); len(list) != 0 {
		t.Fatalf("List after handle Close = %+v, want empty", list)
	}
}

func TestRegistryForwardsHandleSurface(t *testing.T) {
	r := NewSessionRegistry(func(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
		f := newFakeSession(spec.ID)
		f.pid = 7
		f.caps = sandbox.SessionCapabilities{TTY: true, Stdin: true, Signal: true, Events: true}
		return f, nil
	})
	defer func() { _ = r.Close() }()

	sess, err := r.Start(context.Background(), sandbox.SessionSpec{ID: "s", Argv: []string{"sh"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sess.ID() != "s" || sess.PID() != 7 {
		t.Fatalf("handle = %q/%d, want s/7", sess.ID(), sess.PID())
	}
	if !sess.Capabilities().TTY {
		t.Error("the handle does not forward the session's capabilities")
	}
	info, _ := r.List(context.Background())
	if len(info) != 1 || info[0].PID != 7 {
		t.Fatalf("List = %+v, want the session's pid", info)
	}
}

// A starter that refuses must not leave a record behind, or the id it was asked
// for becomes permanently unusable.
func TestRegistryRemovesFailedStart(t *testing.T) {
	r := NewSessionRegistry(func(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
		return nil, errdefs.NotAvailablef("no sessions here")
	})
	defer func() { _ = r.Close() }()

	if _, err := r.Start(context.Background(), sandbox.SessionSpec{ID: "f", Argv: []string{"sh"}}); err == nil {
		t.Fatal("Start succeeded with a refusing starter")
	}
	if list, _ := r.List(context.Background()); len(list) != 0 {
		t.Fatalf("List = %+v, want the failed record removed", list)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSessionStarterNotConfigured(t *testing.T) {
	r := NewSessionRegistry(nil)
	if _, err := r.Start(context.Background(), sandbox.SessionSpec{Argv: []string{"sh"}}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start without a starter = %v, want NotAvailable", err)
	}
}
