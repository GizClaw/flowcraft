package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

// newDeleteManager builds a Manager with resume enabled over a
// checkpoint store the test can inspect.
func newDeleteManager(
	t *testing.T,
	engine agent.Engine,
	store agent.CheckpointStore,
) *Manager {
	t.Helper()
	bus := event.NewMemoryBus()
	router := event.NewRouter(bus)
	instance := &agent.Agent{ID: "agent-a", Engine: engine}
	manager, err := NewManager(
		&testResolver{instances: map[string]*agent.Agent{"agent-a": instance}},
		HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
			return agent.HostFuncs{Inner: testHost{bus: bus}}, nil
		}),
		router,
		WithIdleTimeout(time.Minute),
		WithSinkBufferSize(8),
		WithCheckpointStore(store),
		WithResume(true),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	return manager
}

const deleteAgentID = "agent-a"

func deleteKey(contextID string) Key {
	return Key{AgentID: deleteAgentID, ContextID: contextID}
}

func storeKeys(store *resumeTestStore) []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	keys := make([]string, 0, len(store.cps))
	for key := range store.cps {
		keys = append(keys, key)
	}
	return keys
}

func TestManagerDeleteSessionRemovesCommittedState(t *testing.T) {
	engine := &resumeProbeEngine{reply: "hello"}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, engine, store)
	key := deleteKey("ctx")

	lease, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("lease.Close: %v", err)
	}

	stateID := sessionStateID(key)
	if _, ok := store.cps[stateID]; !ok {
		t.Fatalf("committed turn did not persist session state under %q", stateID)
	}

	if err := manager.DeleteSession(context.Background(), key); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if keys := storeKeys(store); len(keys) != 0 {
		t.Fatalf("checkpoint store still holds %v after delete", keys)
	}
	if err := manager.DeleteSession(context.Background(), key); err != nil {
		t.Fatalf("second DeleteSession: %v", err)
	}

	// A new Open must start a fresh session: no persisted history.
	fresh, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate after delete: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	engine.reply = "fresh"
	turn, err = fresh.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "again")})
	if err != nil {
		t.Fatalf("fresh Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("fresh Wait: %v", err)
	}
	got := engine.snapshot()
	msgs := got.board.Channels[agent.MainChannel]
	if len(msgs) != 1 {
		t.Fatalf("fresh board has %d messages, want 1 (no seeded history)", len(msgs))
	}
	if runID, ok, err := fresh.Session().Resumable(context.Background()); err != nil || ok {
		t.Fatalf("fresh Resumable = (%q, %v, %v), want no parked run", runID, ok, err)
	}
}

func TestManagerDeleteSessionRemovesParkedRun(t *testing.T) {
	engine := &resumeProbeEngine{reply: "hello", interrupt: true}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, engine, store)
	key := deleteKey("ctx")

	lease, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Status != agent.StatusInterrupted {
		t.Fatalf("Status = %q, want interrupted", result.Status)
	}
	runID := turn.RunID()
	if err := lease.Close(); err != nil {
		t.Fatalf("lease.Close: %v", err)
	}

	// A real engine checkpoints during execution; the probe engine does
	// not, so seed the parked run's checkpoint the way resume_test does.
	store.cps[runID] = agent.Checkpoint{
		ExecID:  runID,
		Board:   agent.NewBoard().Snapshot(),
		Payload: []byte(`{"resumable_run_id":"` + runID + `"}`),
	}
	if _, ok := store.cps[sessionStateID(key)]; !ok {
		t.Fatal("interrupted turn did not persist session state")
	}

	if err := manager.DeleteSession(context.Background(), key); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if keys := storeKeys(store); len(keys) != 0 {
		t.Fatalf("checkpoint store still holds %v after delete", keys)
	}
}

func TestManagerDeleteSessionDrainsActiveTurnAndTimesOut(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, blockingEngine(release), store)
	key := deleteKey("ctx")
	stateID := sessionStateID(key)
	// Pre-seed a durable session record so the timed-out delete can be
	// observed not to touch it.
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "seed"))
	store.cps[stateID] = agent.Checkpoint{
		ExecID:  stateID,
		Board:   board.Snapshot(),
		Payload: []byte(`{"resumable_run_id":"run-kept"}`),
	}

	lease, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- manager.DeleteSession(drainCtx, key)
	}()
	deadline := time.Now().Add(time.Second)
	for !manager.keyDeleting(key) {
		if time.Now().After(deadline) {
			t.Fatal("delete did not start within deadline")
		}
		time.Sleep(time.Millisecond)
	}
	// Opens are refused while the delete is in flight.
	if _, err := manager.Open(context.Background(), key); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Open during delete error = %v, want not available", err)
	}
	err = <-deleteDone
	if !errdefs.IsTimeout(err) {
		t.Fatalf("DeleteSession with busy turn = %v, want timeout", err)
	}

	// Rolled back: the delete left both the session and its state intact,
	// and the key is openable again.
	if _, ok := store.cps[stateID]; !ok {
		t.Fatal("timed-out delete removed persisted session state")
	}
	if again, err := manager.Open(context.Background(), key); err != nil {
		t.Fatalf("Open after timed-out delete: %v", err)
	} else {
		_ = again.Close()
	}

	releaseOnce.Do(func() { close(release) })
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := manager.DeleteSession(context.Background(), key); err != nil {
		t.Fatalf("retry DeleteSession: %v", err)
	}
	if keys := storeKeys(store); len(keys) != 0 {
		t.Fatalf("checkpoint store still holds %v after retried delete", keys)
	}
}

func TestManagerDeleteSessionClosesLiveSession(t *testing.T) {
	engine := &resumeProbeEngine{reply: "hello"}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, engine, store)
	key := deleteKey("ctx")

	lease, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if err := manager.DeleteSession(context.Background(), key); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := lease.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
	); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Start on deleted session error = %v, want ErrSessionClosed", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("lease.Close: %v", err)
	}

	fresh, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate after delete: %v", err)
	}
	defer func() { _ = fresh.Close() }()
}

func TestManagerDeleteSessionUnknownKeyIsNoop(t *testing.T) {
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, &resumeProbeEngine{}, store)
	if err := manager.DeleteSession(context.Background(), deleteKey("never-opened")); err != nil {
		t.Fatalf("DeleteSession for unknown key: %v", err)
	}
	if keys := storeKeys(store); len(keys) != 0 {
		t.Fatalf("checkpoint store mutated by no-op delete: %v", keys)
	}
}

func TestManagerDeleteSessionValidation(t *testing.T) {
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, &resumeProbeEngine{}, store)

	if err := manager.DeleteSession(context.Background(), Key{}); !errdefs.IsValidation(err) {
		t.Fatalf("DeleteSession with invalid key = %v, want validation", err)
	}
	//nolint:staticcheck // deliberate: nil Context must be rejected
	if err := manager.DeleteSession(nil, deleteKey("ctx")); !errdefs.IsValidation(err) {
		t.Fatalf("DeleteSession with nil ctx = %v, want validation", err)
	}
}

func TestManagerDeleteSessionAgentScoped(t *testing.T) {
	engine := &resumeProbeEngine{reply: "hello"}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, engine, store)

	key := deleteKey("ctx")
	other := Key{AgentID: deleteAgentID, ContextID: "other"}
	lease, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("lease.Close: %v", err)
	}

	otherLease, err := manager.GetOrCreate(context.Background(), other)
	if err != nil {
		t.Fatalf("GetOrCreate(other): %v", err)
	}
	defer func() { _ = otherLease.Close() }()
	turn, err = otherLease.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("other Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("other Wait: %v", err)
	}

	if err := manager.DeleteSession(context.Background(), key); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, ok := store.cps[sessionStateID(other)]; !ok {
		t.Fatal("deleting one session removed another key's state")
	}
}

func TestManagerDeleteSessionConcurrentDeleteRejected(t *testing.T) {
	release := make(chan struct{})
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, blockingEngine(release), store)
	key := deleteKey("ctx")

	lease, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done <- manager.DeleteSession(ctx, key)
	}()
	// Wait until the first delete is in flight.
	deadline := time.Now().Add(time.Second)
	for !manager.keyDeleting(key) {
		if time.Now().After(deadline) {
			t.Fatal("delete did not start within deadline")
		}
		time.Sleep(time.Millisecond)
	}
	err = manager.DeleteSession(context.Background(), key)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("concurrent DeleteSession = %v, want not available", err)
	}

	close(release)
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("in-flight DeleteSession: %v", err)
	}
}

// ignoreCtxEngine blocks until release, ignoring context cancellation, so
// Session.close's shutdown wait times out with the turn still running.
func ignoreCtxEngine(release <-chan struct{}) agent.Engine {
	return agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		<-release
		return board, nil
	})
}

func TestManagerDeleteSessionKeepsStateWhenCloseTimesOut(t *testing.T) {
	release := make(chan struct{})
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, ignoreCtxEngine(release), store)
	key := deleteKey("ctx")
	stateID := sessionStateID(key)
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "seed"))
	store.cps[stateID] = agent.Checkpoint{
		ExecID:  stateID,
		Board:   board.Snapshot(),
		Payload: []byte(`{}`),
	}

	originalCloseTimeout := sessionCloseTurnTimeout
	sessionCloseTurnTimeout = 100 * time.Millisecond
	t.Cleanup(func() { sessionCloseTurnTimeout = originalCloseTimeout })

	lease, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Simulate the drain->close race: the turn starts after the drain
	// observed idle, then close()'s shutdown wait expires with the turn
	// still running (close swallows the timeout and returns nil).
	if err := lease.Session().close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !lease.Session().hasActiveTurn() {
		t.Fatal("turn stopped within close budget; test cannot exercise the timeout path")
	}

	// DeleteSession must refuse to delete durable state while the turn
	// could still write it, and must keep the session observable for a
	// retry.
	err = manager.DeleteSession(context.Background(), key)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("DeleteSession with active closing turn = %v, want not available", err)
	}
	if _, ok := store.cps[stateID]; !ok {
		t.Fatal("DeleteSession removed durable state while the turn was still active")
	}
	if _, err := manager.Open(context.Background(), key); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Open on closing session = %v, want not available", err)
	}

	close(release)
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// The finished turn writes its terminal state; a retry must now
	// complete the deletion.
	if err := manager.DeleteSession(context.Background(), key); err != nil {
		t.Fatalf("retry DeleteSession: %v", err)
	}
	if keys := storeKeys(store); len(keys) != 0 {
		t.Fatalf("checkpoint store still holds %v after retried delete", keys)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("lease.Close: %v", err)
	}
}

func (m *Manager) keyDeleting(key Key) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.deleting[key]
	return ok
}

func TestDeleteSessionRemovesResumableRequest(t *testing.T) {
	engine := &resumeProbeEngine{reply: "hello", interrupt: true}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	manager := newDeleteManager(t, engine, store)
	key := deleteKey("ctx")

	lease, err := manager.GetOrCreate(context.Background(), key)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	turn, err := lease.Session().Start(context.Background(), agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
		Inputs:  map[string]any{"model": "deepseek-v4-flash"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("lease.Close: %v", err)
	}

	stateID := sessionStateID(key)
	cp, err := store.Load(context.Background(), stateID)
	if err != nil || cp == nil {
		t.Fatalf("Load session state = (%v, %v), want record", cp, err)
	}
	if !strings.Contains(string(cp.Payload), `"resumable_run_id"`) {
		t.Fatalf("session state payload lacks resumable run marker: %s", cp.Payload)
	}

	if err := manager.DeleteSession(context.Background(), key); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if keys := storeKeys(store); len(keys) != 0 {
		t.Fatalf("checkpoint store still holds %v after delete", keys)
	}
}
