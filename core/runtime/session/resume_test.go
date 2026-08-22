package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestSessionStateIDIsDeterministicPerKey(t *testing.T) {
	first := sessionStateID(Key{AgentID: "agent-a", ContextID: "ctx"})
	second := sessionStateID(Key{AgentID: "agent-a", ContextID: "ctx"})
	other := sessionStateID(Key{AgentID: "agent-a", ContextID: "other"})
	if first != second {
		t.Fatalf("session state id changed for same key: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("session state id collided across contexts: %q", first)
	}
}

func TestSessionResume_FreshStartUsesFreshRunID(t *testing.T) {
	engine := &resumeProbeEngine{reply: "hello"}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	req := agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")}
	first, err := session.Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(first.RunID(), "run-") {
		t.Fatalf("RunID = %q, want run- prefix", first.RunID())
	}
	if _, err := first.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	second, err := session.Start(context.Background(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if second.RunID() == first.RunID() {
		t.Fatalf("second turn reused run id %q", second.RunID())
	}
	if _, err := second.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := engine.snapshot()
	if got.resume != nil {
		t.Fatalf("fresh start got ResumeFrom: %+v", got.resume)
	}
	if got.resumeCtx != nil {
		t.Fatalf("fresh start got ResumeContext: %+v", got.resumeCtx)
	}
}

func TestSessionResume_NewTurnCarriesCommittedBoard(t *testing.T) {
	engine := &resumeProbeEngine{reply: "first"}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	first, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := first.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// The committed turn must delete its run checkpoint and park nothing.
	store.mu.Lock()
	deleted := append([]string(nil), store.deleted...)
	store.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != first.RunID() {
		t.Fatalf("deleted = %v, want [%s]", deleted, first.RunID())
	}
	if runID, ok, err := session.Resumable(context.Background()); err != nil || ok {
		t.Fatalf("Resumable = (%q, %v, %v), want no parked run", runID, ok, err)
	}

	second, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "again")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if second.RunID() == first.RunID() {
		t.Fatalf("second turn reused run id %q", second.RunID())
	}
	if _, err := second.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := engine.snapshot()
	if got.resume != nil {
		t.Fatalf("new turn must not resume: %+v", got.resume)
	}
	msgs := got.board.Channels[agent.MainChannel]
	if len(msgs) != 3 {
		t.Fatalf("main channel has %d messages, want 3 (history hi/first + new again)", len(msgs))
	}
	if msgs[0].Role != message.RoleUser || msgs[0].Content.Text() != "hi" {
		t.Fatalf("msgs[0] = %+v, want user 'hi'", msgs[0])
	}
	if msgs[1].Role != message.RoleAssistant || msgs[1].Content.Text() != "first" {
		t.Fatalf("msgs[1] = %+v, want assistant 'first'", msgs[1])
	}
	if msgs[2].Role != message.RoleUser || msgs[2].Content.Text() != "again" {
		t.Fatalf("msgs[2] = %+v, want user 'again'", msgs[2])
	}
}

func TestSessionResume_NewTurnAfterInterruptStartsFresh(t *testing.T) {
	engine := &resumeProbeEngine{interrupt: true}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	first, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := first.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Status != agent.StatusInterrupted {
		t.Fatalf("Status = %q, want interrupted", result.Status)
	}
	firstRunID := first.RunID()
	if runID, ok, err := session.Resumable(context.Background()); err != nil || !ok || runID != firstRunID {
		t.Fatalf("Resumable = (%q, %v, %v), want parked %q", runID, ok, err, firstRunID)
	}

	// A new user message starts a fresh run; it must NOT resume the
	// parked one, and it starts from the last COMMITTED board (empty
	// here, because the interrupted run never committed).
	engine.interrupt = false
	engine.reply = "ok"
	second, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "again")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if second.RunID() == firstRunID {
		t.Fatalf("new turn reused parked run id %q", firstRunID)
	}
	if _, err := second.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	got := engine.snapshot()
	if got.resume != nil {
		t.Fatalf("new turn must not resume the parked run: %+v", got.resume)
	}
	msgs := got.board.Channels[agent.MainChannel]
	if len(msgs) != 1 {
		t.Fatalf("main channel has %d messages, want 1 (interrupted board is not committed)", len(msgs))
	}
	if msgs[0].Content.Text() != "again" {
		t.Fatalf("msgs[0] = %+v, want user 'again'", msgs[0])
	}

	// The new committed turn moves the conversation forward: the old
	// parked marker is cleared.
	if runID, ok, err := session.Resumable(context.Background()); err != nil || ok {
		t.Fatalf("Resumable after commit = (%q, %v, %v), want cleared", runID, ok, err)
	}
}

func TestSessionResume_ResumeReplaysParkedRunAndDeletesOnCommit(t *testing.T) {
	engine := &resumeProbeEngine{reply: "done"}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	runID := "run-" + strings.Repeat("a", 16)
	originalStart := time.Now().Add(-2 * time.Hour)
	parkRun(t, session, store, runID, originalStart,
		&agent.Request{TaskID: "task-1", Message: message.NewTextMessage(message.RoleUser, "continue")})

	turn, err := session.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if turn.RunID() != runID {
		t.Fatalf("RunID = %q, want parked %q", turn.RunID(), runID)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := engine.snapshot()
	if got.resume == nil || got.resume.ExecID != runID || len(got.resume.Steps) != 1 {
		t.Fatalf("engine ResumeFrom = %+v, want run %s checkpoint", got.resume, runID)
	}
	if got.resumeCtx == nil || got.resumeCtx.Attempt < 2 || got.resumeCtx.Signal != "resume" ||
		!got.resumeCtx.StartedAt.Equal(originalStart) {
		t.Fatalf("engine ResumeContext = %+v, want resume metadata", got.resumeCtx)
	}
	if got.taskID != "task-1" {
		t.Fatalf("TaskID = %q, want task-1 (original request must be replayed)", got.taskID)
	}

	store.mu.Lock()
	deleted := append([]string(nil), store.deleted...)
	store.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != runID {
		t.Fatalf("deleted = %v, want [%s]", deleted, runID)
	}
	if runID, ok, err := session.Resumable(context.Background()); err != nil || ok {
		t.Fatalf("Resumable after commit = (%q, %v, %v), want cleared", runID, ok, err)
	}
}

func TestSessionResume_ResumeKeepsCheckpointWhenInterrupted(t *testing.T) {
	engine := &resumeProbeEngine{interrupt: true}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	runID := "run-" + strings.Repeat("b", 16)
	parkRun(t, session, store, runID, time.Now().Add(-time.Hour),
		&agent.Request{Message: message.NewTextMessage(message.RoleUser, "continue")})

	turn, err := session.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Status != agent.StatusInterrupted {
		t.Fatalf("Status = %q, want interrupted", result.Status)
	}
	store.mu.Lock()
	deleted := append([]string(nil), store.deleted...)
	_, kept := store.cps[runID]
	store.mu.Unlock()
	if len(deleted) != 0 {
		t.Fatalf("interrupted resume deleted checkpoint: %v", deleted)
	}
	if !kept {
		t.Fatalf("checkpoint %s was removed", runID)
	}
	if runID, ok, err := session.Resumable(context.Background()); err != nil || !ok || runID != turn.RunID() {
		t.Fatalf("Resumable = (%q, %v, %v), want parked %q", runID, ok, err, turn.RunID())
	}
}

func TestSessionResume_ResumeWithoutParkedRun(t *testing.T) {
	engine := &resumeProbeEngine{}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	if _, err := session.Resume(context.Background()); !errdefs.IsNotFound(err) {
		t.Fatalf("Resume error = %v, want NotFound", err)
	}
}

func TestSessionResume_ParkedRequest(t *testing.T) {
	engine := &resumeProbeEngine{reply: "done"}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	// Fresh session: nothing parked.
	if req, ok, err := session.ParkedRequest(context.Background()); err != nil || ok {
		t.Fatalf("ParkedRequest before park = (%+v, %v, %v), want no request", req, ok, err)
	}

	runID := "run-" + strings.Repeat("p", 16)
	parkRun(t, session, store, runID, time.Now().Add(-time.Hour),
		&agent.Request{
			TaskID:  "task-9",
			Message: message.NewTextMessage(message.RoleUser, "continue"),
			Inputs:  map[string]any{"pipeline": "image"},
		})

	got, ok, err := session.ParkedRequest(context.Background())
	if err != nil || !ok {
		t.Fatalf("ParkedRequest after park = (%+v, %v, %v), want parked request", got, ok, err)
	}
	if got.TaskID != "task-9" || got.Message.Content.Text() != "continue" {
		t.Fatalf("ParkedRequest = %+v, want task-9 / 'continue'", got)
	}
	if got.Inputs["pipeline"] != "image" {
		t.Fatalf("ParkedRequest.Inputs = %+v, want pipeline=image", got.Inputs)
	}

	// A committed resume clears the parked marker and request.
	turn, err := session.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if req, ok, err := session.ParkedRequest(context.Background()); err != nil || ok {
		t.Fatalf("ParkedRequest after commit = (%+v, %v, %v), want cleared", req, ok, err)
	}
}

func TestSessionResume_RejectsNonResumableEngine(t *testing.T) {
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	})
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	runID := "run-" + strings.Repeat("c", 16)
	parkRun(t, session, store, runID, time.Now().Add(-time.Hour),
		&agent.Request{Message: message.NewTextMessage(message.RoleUser, "continue")})

	if _, err := session.Resume(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Resume error = %v, want NotAvailable", err)
	}
}

func TestManagerResumeRequiresStore(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	router := event.NewRouter(bus)
	defer func() { _ = router.Close() }()
	instance := &agent.Agent{
		ID: "agent-a",
		Engine: agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
			return b, nil
		}),
	}
	_, err := NewManager(
		&testResolver{instances: map[string]*agent.Agent{"agent-a": instance}},
		HostFactoryFunc(func(_ context.Context, _ HostRequest) (agent.Host, error) {
			return agent.NoopHost{}, nil
		}),
		router,
		WithResume(true),
	)
	if !errdefs.IsValidation(err) {
		t.Fatalf("NewManager with resume and no store = %v, want Validation", err)
	}
}

func parkRun(
	t *testing.T,
	session *Session,
	store *resumeTestStore,
	runID string,
	originalStart time.Time,
	req *agent.Request,
) {
	t.Helper()
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "seed"))
	store.cps[runID] = agent.Checkpoint{
		ExecID:            runID,
		Steps:             []string{"wave-1"},
		Board:             board.Snapshot(),
		Timestamp:         time.Now().Add(-time.Hour),
		OriginalStartedAt: originalStart,
		SpecVersion:       "v1",
	}
	session.saveSessionState(&sessionState{
		ResumableRunID: runID,
		Request:        req,
		Board:          agent.NewBoard().Snapshot(),
	}, store, true)
}

type resumeProbeEngine struct {
	mu           sync.Mutex
	gotResume    *agent.Checkpoint
	gotResumeCtx *agent.ResumeContext
	gotBoard     *agent.BoardSnapshot
	gotTaskID    string
	reply        string
	interrupt    bool
}

type resumeProbeSnapshot struct {
	resume    *agent.Checkpoint
	resumeCtx *agent.ResumeContext
	board     *agent.BoardSnapshot
	taskID    string
}

func (e *resumeProbeEngine) snapshot() resumeProbeSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return resumeProbeSnapshot{
		resume:    e.gotResume,
		resumeCtx: e.gotResumeCtx,
		board:     e.gotBoard,
		taskID:    e.gotTaskID,
	}
}

func (e *resumeProbeEngine) Execute(
	ctx context.Context,
	run agent.Run,
	_ agent.Host,
	board *agent.Board,
) (*agent.Board, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if run.ResumeFrom != nil {
		clone := run.ResumeFrom.Clone()
		e.gotResume = &clone
	}
	if rc, ok := agent.ResumeContextFromContext(ctx); ok {
		value := rc
		e.gotResumeCtx = &value
	}
	e.gotBoard = board.Snapshot()
	e.gotTaskID = run.TaskID
	if e.interrupt {
		return board, agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput})
	}
	if e.reply != "" {
		board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, e.reply))
	}
	return board, nil
}

func (*resumeProbeEngine) CanResume(agent.Checkpoint) error { return nil }

type resumeTestStore struct {
	mu      sync.Mutex
	cps     map[string]agent.Checkpoint
	deleted []string
}

func (s *resumeTestStore) Save(_ context.Context, cp agent.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cps[cp.ExecID] = cp.Clone()
	return nil
}

func (s *resumeTestStore) Load(_ context.Context, execID string) (*agent.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.cps[execID]
	if !ok {
		return nil, nil
	}
	clone := cp.Clone()
	return &clone, nil
}

func (s *resumeTestStore) Delete(_ context.Context, execID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cps, execID)
	s.deleted = append(s.deleted, execID)
	return nil
}

func newResumeSession(
	t *testing.T,
	engine agent.Engine,
	store agent.CheckpointStore,
) *Session {
	t.Helper()
	bus := event.NewMemoryBus()
	router := event.NewRouter(bus)
	instance := &agent.Agent{
		ID:     "agent-a",
		Engine: engine,
	}
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
	lease, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = lease.Close()
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	return lease.Session()
}
