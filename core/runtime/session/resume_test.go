package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestStableRunIDIsDeterministicPerKey(t *testing.T) {
	first := stableRunID(Key{AgentID: "agent-a", ContextID: "ctx"})
	second := stableRunID(Key{AgentID: "agent-a", ContextID: "ctx"})
	other := stableRunID(Key{AgentID: "agent-a", ContextID: "other"})
	if first != second {
		t.Fatalf("stable run id changed for same key: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("stable run id collided across contexts: %q", first)
	}
}

func TestSessionResume_FreshStartUsesStableRunID(t *testing.T) {
	engine := &resumeProbeEngine{}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	turn, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantRunID := stableRunID(Key{AgentID: "agent-a", ContextID: "ctx"})
	if turn.RunID() != wantRunID {
		t.Fatalf("RunID = %q, want stable %q", turn.RunID(), wantRunID)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.gotResume != nil {
		t.Fatalf("fresh start got ResumeFrom: %+v", engine.gotResume)
	}
	if engine.gotResumeCtx != nil {
		t.Fatalf("fresh start got ResumeContext: %+v", engine.gotResumeCtx)
	}
}

func TestSessionResume_ResumesFromCheckpointAndDeletesOnCommit(t *testing.T) {
	engine := &resumeProbeEngine{}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	runID := stableRunID(Key{AgentID: "agent-a", ContextID: "ctx"})
	originalStart := time.Now().Add(-2 * time.Hour)
	store.cps[runID] = agent.Checkpoint{
		ExecID:            runID,
		Steps:             []string{"wave-1"},
		Board:             agent.NewBoard().Snapshot(),
		Timestamp:         time.Now().Add(-time.Hour),
		OriginalStartedAt: originalStart,
		SpecVersion:       "v1",
	}

	turn, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "continue")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	engine.mu.Lock()
	gotResume := engine.gotResume
	gotCtx := engine.gotResumeCtx
	engine.mu.Unlock()
	if gotResume == nil || gotResume.ExecID != runID || len(gotResume.Steps) != 1 {
		t.Fatalf("engine ResumeFrom = %+v, want run %s checkpoint", gotResume, runID)
	}
	if gotCtx == nil || gotCtx.Attempt < 2 || gotCtx.Signal != "session" ||
		!gotCtx.StartedAt.Equal(originalStart) {
		t.Fatalf("engine ResumeContext = %+v, want session resume metadata", gotCtx)
	}

	store.mu.Lock()
	deleted := append([]string(nil), store.deleted...)
	store.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != runID {
		t.Fatalf("deleted = %v, want [%s]", deleted, runID)
	}
}

func TestSessionResume_KeepsCheckpointWhenInterrupted(t *testing.T) {
	engine := &resumeProbeEngine{interrupt: true}
	store := &resumeTestStore{cps: make(map[string]agent.Checkpoint)}
	session := newResumeSession(t, engine, store)

	runID := stableRunID(Key{AgentID: "agent-a", ContextID: "ctx"})
	store.cps[runID] = agent.Checkpoint{
		ExecID: runID,
		Steps:  []string{"wave-1"},
		Board:  agent.NewBoard().Snapshot(),
	}

	turn, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "again")})
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
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deleted) != 0 {
		t.Fatalf("interrupted run deleted checkpoint: %v", store.deleted)
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

	runID := stableRunID(Key{AgentID: "agent-a", ContextID: "ctx"})
	store.cps[runID] = agent.Checkpoint{
		ExecID: runID,
		Steps:  []string{"wave-1"},
		Board:  agent.NewBoard().Snapshot(),
	}
	if _, err := session.Start(context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start error = %v, want NotAvailable", err)
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

type resumeProbeEngine struct {
	mu           sync.Mutex
	gotResume    *agent.Checkpoint
	gotResumeCtx *agent.ResumeContext
	interrupt    bool
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
	if e.interrupt {
		return board, agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput})
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
