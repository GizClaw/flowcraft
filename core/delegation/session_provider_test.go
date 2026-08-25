package delegation

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/telemetry"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type testSessionProvider struct {
	contextID  string
	err        error
	persistent bool
}

func (p *testSessionProvider) CreateContextID(context.Context, AsyncRequest) (string, error) {
	return p.contextID, p.err
}

func (p *testSessionProvider) Persistent() bool { return p.persistent }

type delegationTestResolver struct {
	result *deploy.Result
}

func (r delegationTestResolver) Instance(id string) (*agent.Agent, bool) {
	return r.result.Agent(id)
}

// nilEngineResolver serves an agent with no engine so agent.Execute fails
// with an infrastructure validation error inside the turn.
type nilEngineResolver struct{}

func (nilEngineResolver) Instance(id string) (*agent.Agent, bool) {
	return &agent.Agent{ID: id}, true
}

type delegationTestHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h delegationTestHost) Publish(ctx context.Context, env event.Envelope) error {
	return h.bus.Publish(ctx, env)
}

// runEndEngine wraps an engine so it publishes the run-end event the
// session stream coordinator waits for. Unwrap keeps decorated
// capabilities (e.g. Resumer) visible to host-side probes.
type runEndEngine struct {
	inner agent.Engine
}

func (e runEndEngine) Execute(
	ctx context.Context,
	run agent.Run,
	host agent.Host,
	board *agent.Board,
) (*agent.Board, error) {
	board, err := e.inner.Execute(ctx, run, host, board)
	publishCtx := context.WithoutCancel(ctx)
	envelope, envelopeErr := event.NewEnvelope(
		publishCtx, agent.SubjectRunEnd(run.RunID), nil)
	if envelopeErr == nil {
		envelope.SetRunID(run.RunID)
		envelopeErr = host.Publish(publishCtx, envelope)
	}
	if err != nil {
		return board, err
	}
	return board, envelopeErr
}

func (e runEndEngine) Unwrap() agent.Engine { return e.inner }

// delegationWithRunEnd wraps an engine so it publishes the run-end event
// the session stream coordinator waits for.
func delegationWithRunEnd(engine agent.Engine) agent.Engine {
	return runEndEngine{inner: engine}
}

func newTestSessionManagerForResult(
	t *testing.T,
	result *deploy.Result,
	options ...session.ManagerOption,
) *session.Manager {
	return newTestSessionManagerForResolver(
		t, delegationTestResolver{result: result}, options...)
}

func newTestSessionManagerForResolver(
	t *testing.T,
	resolver session.InstanceResolver,
	options ...session.ManagerOption,
) *session.Manager {
	t.Helper()
	bus := event.NewMemoryBus()
	router := event.NewRouter(bus)
	factory := session.HostFactoryFunc(func(
		_ context.Context,
		request session.HostRequest,
	) (agent.Host, error) {
		return agent.HostFuncs{
			Inner:        delegationTestHost{bus: bus},
			InterruptsFn: func() <-chan agent.Interrupt { return request.Interrupts },
			AskUserFn:    request.AskUser,
		}, nil
	})
	manager, err := session.NewManager(
		resolver,
		factory,
		router,
		append([]session.ManagerOption{
			session.WithIdleTimeout(time.Minute),
			session.WithSinkBufferSize(8),
		}, options...)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	return manager
}

func TestWaitTurnCancelOnDoneSurfacesInfraError(t *testing.T) {
	manager := newTestSessionManagerForResolver(t, nilEngineResolver{})
	ctx := context.Background()
	lease, err := manager.GetOrCreate(ctx, session.Key{
		AgentID: "writer", ContextID: "infra-ctx",
	})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().StartWithOptions(ctx, agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("StartWithOptions: %v", err)
	}
	result, err := waitTurnCancelOnDone(ctx, turn)
	if err == nil {
		t.Fatalf("waitTurnCancelOnDone = %+v, nil error; want infra error", result)
	}
	if strings.Contains(err.Error(), "wait for canceled turn") {
		t.Fatalf("infra error was relabeled as a canceled wait: %v", err)
	}
	if !strings.Contains(err.Error(), "nil engine") {
		t.Fatalf("error = %v, want the raw nil-engine failure", err)
	}
	if response := canceledOrFailedResponse(err); response.Status != StatusFailed {
		t.Fatalf("response status = %q, want failed", response.Status)
	}
}

func TestRunAtSessionPathInfraErrorSurfacesRaw(t *testing.T) {
	result := buildResult(t, completedEngine("ok"))
	directory := NewDirectory()
	if err := directory.Bind(result); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	manager := newTestSessionManagerForResolver(t, nilEngineResolver{})
	if err := service.BindSessionManager(manager); err != nil {
		t.Fatal(err)
	}
	response, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusFailed {
		t.Fatalf("response status = %q, want failed", response.Status)
	}
	if strings.Contains(response.Error, "wait for canceled turn") {
		t.Fatalf("infra error was relabeled as a canceled wait: %v", response.Error)
	}
	if !strings.Contains(response.Error, "nil engine") {
		t.Fatalf("response error = %q, want the raw nil-engine failure", response.Error)
	}
}

// newSessionPathService builds a service bound to a session manager over
// the same deployment result as its directory.
func newSessionPathService(
	t *testing.T,
	engine agent.Engine,
	provider SessionProvider,
	managerOptions ...session.ManagerOption,
) (*LocalService, *deploy.Result) {
	t.Helper()
	result := buildResult(t, delegationWithRunEnd(engine))
	directory := NewDirectory()
	if err := directory.Bind(result); err != nil {
		t.Fatal(err)
	}
	options := []Option{}
	if provider != nil {
		options = append(options, WithSessionProvider(provider))
	}
	service, err := NewService(directory, nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	manager := newTestSessionManagerForResult(t, result, managerOptions...)
	if err := service.BindSessionManager(manager); err != nil {
		t.Fatal(err)
	}
	return service, result
}

func TestRunAtSessionPathSetsContextIDAndSessionID(t *testing.T) {
	var gotContextID string
	engine := agent.EngineFunc(func(
		_ context.Context,
		run agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		gotContextID = run.ConversationID
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	service, _ := newSessionPathService(
		t, engine, &testSessionProvider{contextID: "stable-ctx", persistent: true})
	response, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	if response.Metadata["delegation.session_id"] != "stable-ctx" {
		t.Fatalf("session_id = %q, want stable-ctx", response.Metadata["delegation.session_id"])
	}
	if gotContextID != "stable-ctx" {
		t.Fatalf("subagent ContextID = %q, want stable-ctx", gotContextID)
	}
}

func TestRunAtSessionPathDefaultMint(t *testing.T) {
	var gotContextID string
	engine := agent.EngineFunc(func(
		_ context.Context,
		run agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		gotContextID = run.ConversationID
		return board, nil
	})
	service, _ := newSessionPathService(t, engine, nil)
	response, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	sessionID := response.Metadata["delegation.session_id"]
	if response.Status != StatusSucceeded || sessionID == "" {
		t.Fatalf("response = %+v, want succeeded with a session id", response)
	}
	if gotContextID != sessionID {
		t.Fatalf("subagent ContextID = %q, want %q", gotContextID, sessionID)
	}
}

func TestRunAtLegacyWithoutManagerKeepsEmptyContextID(t *testing.T) {
	var gotContextID string
	engine := agent.EngineFunc(func(
		_ context.Context,
		run agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		gotContextID = run.ConversationID
		return board, nil
	})
	directory := boundDirectory(t, engine)
	service, err := NewService(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	if _, err := service.Delegate(context.Background(), syncRequest("writer")); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if gotContextID != "" {
		t.Fatalf("legacy subagent ContextID = %q, want empty", gotContextID)
	}
}

func TestRunAtProviderWithoutManagerSetsContextID(t *testing.T) {
	var gotContextID string
	engine := agent.EngineFunc(func(
		_ context.Context,
		run agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		gotContextID = run.ConversationID
		return board, nil
	})
	directory := boundDirectory(t, engine)
	service, err := NewService(directory, nil,
		WithSessionProvider(&testSessionProvider{contextID: "ctx-1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	if _, err := service.Delegate(context.Background(), syncRequest("writer")); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if gotContextID != "ctx-1" {
		t.Fatalf("legacy subagent ContextID = %q, want ctx-1", gotContextID)
	}
}

func TestRunAtSessionProviderErrorFails(t *testing.T) {
	service, _ := newSessionPathService(t, completedEngine("ok"),
		&testSessionProvider{err: errdefs.Internalf("provider down")})
	if _, err := service.Delegate(context.Background(), syncRequest("writer")); err == nil {
		t.Fatal("Delegate with failing provider = nil error, want error")
	}
}

func TestRunAtSessionProviderEmptyContextIDRejected(t *testing.T) {
	service, _ := newSessionPathService(t, completedEngine("ok"),
		&testSessionProvider{contextID: ""})
	if _, err := service.Delegate(context.Background(), syncRequest("writer")); !errdefs.IsValidation(err) {
		t.Fatalf("Delegate with empty context id = %v, want validation", err)
	}
}

func TestRunAtSessionPathCanceled(t *testing.T) {
	started := make(chan struct{})
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		close(started)
		<-ctx.Done()
		return board, ctx.Err()
	})
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "cancel-ctx", persistent: true})
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		response Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := service.Delegate(ctx, syncRequest("writer"))
		done <- result{response: response, err: err}
	}()
	<-started
	cancel()
	got := <-done
	if got.err != nil {
		t.Fatalf("Delegate: %v", got.err)
	}
	if got.response.Status != StatusCanceled {
		t.Fatalf("response status = %q, want canceled", got.response.Status)
	}
}

func TestRunAtSessionPathPreservesDepth(t *testing.T) {
	var gotDepth int
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		gotDepth = metadataFromContext(ctx).depth
		return board, nil
	})
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "depth-ctx", persistent: true})
	response, err := service.runAt(context.Background(), AsyncRequest{
		Request: Request{Mode: ModeSync, Target: "writer", Input: "do it"},
		Caller:  "parent",
		Depth:   3,
	}, false)
	if err != nil {
		t.Fatalf("runAt: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	if gotDepth != 3 {
		t.Fatalf("subagent depth = %d, want 3 (metadata must cross the session boundary)", gotDepth)
	}
}

func TestRunAtSessionPathMaxDepthEnforced(t *testing.T) {
	service, _ := newSessionPathService(t, completedEngine("ok"),
		&testSessionProvider{contextID: "depth-ctx", persistent: true})
	service.maxDepth = 1
	if _, err := service.runAt(context.Background(), AsyncRequest{
		Request: Request{Mode: ModeSync, Target: "writer", Input: "do it"},
		Depth:   2,
	}, false); !errdefs.IsPolicyDenied(err) {
		t.Fatalf("runAt over depth = %v, want policy denied", err)
	}
}

func TestRunAtSameKeySecondDelegationInterruptsFirst(t *testing.T) {
	started := make(chan struct{}, 2)
	var runs atomic.Int64
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		started <- struct{}{}
		if runs.Add(1) > 1 {
			return board, nil
		}
		select {
		case interrupt := <-host.Interrupts():
			return board, agent.Interrupted(interrupt)
		case <-ctx.Done():
			return board, ctx.Err()
		}
	})
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "same-ctx", persistent: true})

	first := make(chan Response, 1)
	go func() {
		response, err := service.Delegate(context.Background(), syncRequest("writer"))
		if err != nil {
			t.Errorf("first Delegate: %v", err)
			return
		}
		first <- response
	}()
	<-started
	second, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("second Delegate: %v", err)
	}
	firstResponse := <-first
	if firstResponse.Status != StatusCanceled {
		t.Fatalf("first response status = %q, want canceled", firstResponse.Status)
	}
	if second.Status != StatusSucceeded {
		t.Fatalf("second response status = %q, want succeeded", second.Status)
	}
}

func TestRunAtSessionPathAsyncRun(t *testing.T) {
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "async done"))
		return board, nil
	})
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "async-ctx", persistent: true})
	response, err := service.Run(context.Background(), AsyncRequest{
		Request: Request{Mode: ModeAsync, Target: "writer", Input: "do it"},
		Depth:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	if response.Metadata["delegation.session_id"] != "async-ctx" {
		t.Fatalf("session_id = %q, want async-ctx", response.Metadata["delegation.session_id"])
	}
}

// delegationCheckpointStore is a minimal in-memory CheckpointDeleter
// used to exercise the delegation resume path end to end.
type delegationCheckpointStore struct {
	mu             sync.Mutex
	cps            map[string]agent.Checkpoint
	loadFailPrefix string
}

func newDelegationCheckpointStore() *delegationCheckpointStore {
	return &delegationCheckpointStore{cps: make(map[string]agent.Checkpoint)}
}

func (s *delegationCheckpointStore) Save(_ context.Context, cp agent.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cps[cp.ExecID] = cp.Clone()
	return nil
}

func (s *delegationCheckpointStore) Load(_ context.Context, execID string) (*agent.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadFailPrefix != "" && strings.HasPrefix(execID, s.loadFailPrefix) {
		return nil, errdefs.Fmt("delegation test store: read failed for %s", execID)
	}
	cp, ok := s.cps[execID]
	if !ok {
		return nil, nil
	}
	clone := cp.Clone()
	return &clone, nil
}

func (s *delegationCheckpointStore) Delete(_ context.Context, execID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cps, execID)
	return nil
}

// resumeAwareDelegationEngine fails the first attempt (optionally after
// writing a checkpoint) and succeeds on the next, recording whether the
// second attempt was a resume under the original run id.
type resumeAwareDelegationEngine struct {
	mu              sync.Mutex
	attempts        int
	resumed         bool
	runIDs          []string
	store           *delegationCheckpointStore
	writeCheckpoint bool
}

func (e *resumeAwareDelegationEngine) Execute(
	ctx context.Context,
	run agent.Run,
	_ agent.Host,
	board *agent.Board,
) (*agent.Board, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempts++
	e.runIDs = append(e.runIDs, run.RunID)
	if e.attempts == 1 {
		if e.writeCheckpoint {
			if err := e.store.Save(ctx, agent.Checkpoint{
				ExecID:            run.RunID,
				Steps:             []string{"wave-1"},
				Board:             board.Snapshot(),
				Timestamp:         time.Now(),
				OriginalStartedAt: time.Now(),
				SpecVersion:       "v1",
			}); err != nil {
				return board, err
			}
		}
		return board, errdefs.Internalf("first attempt fails")
	}
	e.resumed = run.ResumeFrom != nil
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleAssistant, "done"))
	return board, nil
}

func (*resumeAwareDelegationEngine) CanResume(agent.Checkpoint) error { return nil }

func TestRunAtPersistentRetryResumesParkedRun(t *testing.T) {
	store := newDelegationCheckpointStore()
	engine := &resumeAwareDelegationEngine{store: store, writeCheckpoint: true}
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "resume-ctx", persistent: true},
		session.WithResume(true), session.WithCheckpointStore(store))

	first, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("first Delegate: %v", err)
	}
	if first.Status != StatusFailed {
		t.Fatalf("first status = %q, want failed", first.Status)
	}

	second, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("retry Delegate: %v", err)
	}
	if second.Status != StatusSucceeded {
		t.Fatalf("retry status = %q, want succeeded", second.Status)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.attempts != 2 {
		t.Fatalf("engine attempts = %d, want 2", engine.attempts)
	}
	if !engine.resumed {
		t.Fatal("retry did not resume the parked run")
	}
	if len(engine.runIDs) != 2 || engine.runIDs[0] != engine.runIDs[1] {
		t.Fatalf("retry run ids = %v, want original id replayed on both attempts", engine.runIDs)
	}
}

func TestRunAtPersistentDifferentRequestStartsFresh(t *testing.T) {
	store := newDelegationCheckpointStore()
	engine := &resumeAwareDelegationEngine{store: store, writeCheckpoint: true}
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "fresh-ctx", persistent: true},
		session.WithResume(true), session.WithCheckpointStore(store))

	first, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil || first.Status != StatusFailed {
		t.Fatalf("first = (%+v, %v), want failed", first, err)
	}

	second, err := service.Delegate(context.Background(), Request{
		Mode:   ModeSync,
		Target: "writer",
		Input:  "different job",
	})
	if err != nil {
		t.Fatalf("retry Delegate: %v", err)
	}
	if second.Status != StatusSucceeded {
		t.Fatalf("retry status = %q, want succeeded", second.Status)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.attempts != 2 {
		t.Fatalf("engine attempts = %d, want 2", engine.attempts)
	}
	if engine.resumed {
		t.Fatal("different request must not resume the parked run")
	}
	if len(engine.runIDs) == 2 && engine.runIDs[0] == engine.runIDs[1] {
		t.Fatalf("different request reused parked run id %q", engine.runIDs[0])
	}
}

func TestRunAtPersistentRetryFallsBackWhenCheckpointMissing(t *testing.T) {
	store := newDelegationCheckpointStore()
	engine := &resumeAwareDelegationEngine{store: store}
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "missing-cp", persistent: true},
		session.WithResume(true), session.WithCheckpointStore(store))

	first, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil || first.Status != StatusFailed {
		t.Fatalf("first = (%+v, %v), want failed", first, err)
	}

	second, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("retry Delegate: %v", err)
	}
	if second.Status != StatusSucceeded {
		t.Fatalf("retry status = %q, want succeeded", second.Status)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.attempts != 2 {
		t.Fatalf("engine attempts = %d, want 2", engine.attempts)
	}
	if engine.resumed {
		t.Fatal("retry with a missing checkpoint must fall back to a fresh start")
	}
}

func TestRunAtPersistentRetryFallsBackWhenEngineCannotResume(t *testing.T) {
	store := newDelegationCheckpointStore()
	var attempts int
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		attempts++
		if attempts == 1 {
			if err := store.Save(ctx, agent.Checkpoint{
				ExecID:      run.RunID,
				Steps:       []string{"wave-1"},
				Board:       board.Snapshot(),
				SpecVersion: "v1",
			}); err != nil {
				return board, err
			}
			return board, errdefs.Internalf("first attempt fails")
		}
		return board, nil
	})
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "no-resume-engine", persistent: true},
		session.WithResume(true), session.WithCheckpointStore(store))

	first, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil || first.Status != StatusFailed {
		t.Fatalf("first = (%+v, %v), want failed", first, err)
	}

	second, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("retry Delegate: %v", err)
	}
	if second.Status != StatusSucceeded {
		t.Fatalf("retry status = %q, want succeeded", second.Status)
	}
	if attempts != 2 {
		t.Fatalf("engine attempts = %d, want 2", attempts)
	}
}

func TestRunAtPersistentRetryFallsBackWhenResumeReadFails(t *testing.T) {
	store := newDelegationCheckpointStore()
	store.loadFailPrefix = "run-"
	engine := &resumeAwareDelegationEngine{store: store, writeCheckpoint: true}
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "store-flaky", persistent: true},
		session.WithResume(true), session.WithCheckpointStore(store))

	first, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil || first.Status != StatusFailed {
		t.Fatalf("first = (%+v, %v), want failed", first, err)
	}

	second, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("retry Delegate: %v", err)
	}
	if second.Status != StatusSucceeded {
		t.Fatalf("retry status = %q, want succeeded", second.Status)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.attempts != 2 {
		t.Fatalf("engine attempts = %d, want 2", engine.attempts)
	}
	if engine.resumed {
		t.Fatal("retry with an unreadable checkpoint must fall back to a fresh start")
	}
}

type delegationRecordingStore struct {
	mu    sync.Mutex
	saved int
}

func (s *delegationRecordingStore) Save(context.Context, agent.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved++
	return nil
}

func (s *delegationRecordingStore) Load(context.Context, string) (*agent.Checkpoint, error) {
	return nil, nil
}

func (s *delegationRecordingStore) Delete(context.Context, string) error {
	return nil
}

func (s *delegationRecordingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestRunAtSessionPathEmitsDelegationTelemetry(t *testing.T) {
	var out lockedBuffer
	exporter := telemetry.NewPlainTextExporter(&out)
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	previous := logglobal.GetLoggerProvider()
	logglobal.SetLoggerProvider(provider)
	t.Cleanup(func() {
		logglobal.SetLoggerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	service, _ := newSessionPathService(t,
		completedEngine("ok"),
		&testSessionProvider{contextID: "telemetry-ctx", persistent: true})
	response, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	got := out.String()
	for _, want := range []string{
		"local delegation: run started",
		"agent.id=writer",
		"delegation.target=writer",
		"delegation.mode=sync",
		"delegation.depth=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("telemetry output missing %q:\n%s", want, got)
		}
	}
}

func TestRunAtSessionPathEphemeralWritesNoState(t *testing.T) {
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	})
	store := &delegationRecordingStore{}
	service, _ := newSessionPathService(t, engine, nil,
		session.WithResume(true), session.WithCheckpointStore(store))
	if _, err := service.Delegate(context.Background(), syncRequest("writer")); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if got := store.count(); got != 0 {
		t.Fatalf("ephemeral delegation wrote %d checkpoints, want none", got)
	}
}

func TestRunAtSessionPathPersistentWritesState(t *testing.T) {
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	})
	store := &delegationRecordingStore{}
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "persist-ctx", persistent: true},
		session.WithResume(true), session.WithCheckpointStore(store))
	if _, err := service.Delegate(context.Background(), syncRequest("writer")); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if got := store.count(); got == 0 {
		t.Fatal("persistent delegation wrote no checkpoints, want session state")
	}
}

// streamEngine emits one stream delta on the run's canonical stream
// subject before completing, so an attached sink has something to
// observe.
func streamEngine(output string) agent.Engine {
	return agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		if err := agent.EmitStreamPart(
			ctx, host, run.RunID, "writer.node.work",
			message.TextPart{Text: output}); err != nil {
			return nil, err
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, output))
		return board, nil
	})
}

// awaitStreamText waits until a stream delta carrying the given text
// arrives. Raw/confirmed sinks may observe other run events first, so
// assertions must scan rather than expect a specific first delta.
func awaitStreamText(t *testing.T, received <-chan agent.StreamDeltaPayload, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case delta := <-received:
			if part, ok := delta.Part.(message.TextPart); ok && part.Text == want {
				return
			}
		case <-deadline:
			t.Fatalf("stream delta with text %q never received", want)
		}
	}
}

func TestSyncDelegationInheritsCallerStreamSinks(t *testing.T) {
	received := make(chan agent.StreamDeltaPayload, 8)
	sink := agent.StreamSinkFunc(func(
		_ context.Context,
		_ event.Envelope,
		delta agent.StreamDeltaPayload,
	) error {
		received <- delta
		return nil
	})
	service, _ := newSessionPathService(t, streamEngine("progress"), nil)
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:   "caller",
			Sink: sink,
		}},
		Inheritable: true,
	})
	response, err := service.Delegate(ctx, syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	awaitStreamText(t, received, "progress")
}

func TestSyncDelegationSkipsNonInheritableStreamPolicy(t *testing.T) {
	received := make(chan agent.StreamDeltaPayload, 8)
	sink := agent.StreamSinkFunc(func(
		_ context.Context,
		_ event.Envelope,
		delta agent.StreamDeltaPayload,
	) error {
		received <- delta
		return nil
	})
	service, _ := newSessionPathService(t, streamEngine("progress"), nil)
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:   "caller",
			Sink: sink,
		}},
		Inheritable: false,
	})
	response, err := service.Delegate(ctx, syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	// Delegate returns only after the subagent turn finalizes, and
	// finalize drains then detaches every attached sink — any delivery
	// caused by a buggy attachment would already be queued. The select
	// below is a bounded absence check on that flushed state, not a race.
	select {
	case delta := <-received:
		t.Fatalf("non-inheritable policy still delivered delta: %#v", delta)
	case <-time.After(time.Second):
	}
}

func TestInheritedSinkSpecsDowngradeAuthorityAndAck(t *testing.T) {
	sink := agent.StreamSinkFunc(func(
		context.Context, event.Envelope, agent.StreamDeltaPayload,
	) error {
		return nil
	})
	spec := session.SinkSpec{
		ID:              "authority",
		Sink:            sink,
		QueueSize:       7,
		DeliveryTimeout: time.Minute,
		Visibility:      session.VisibilityConfirmed,
		Authority:       session.AuthorityAuthoritative,
		AckMode:         session.AckExplicit,
		MaxUnacked:      3,
	}
	got := inheritedSinkSpecs([]session.SinkSpec{spec})
	if len(got) != 1 {
		t.Fatalf("inherited specs = %d, want 1", len(got))
	}
	inherited := got[0]
	if inherited.Authority != session.AuthorityObserver {
		t.Fatalf("inherited Authority = %q, want observer", inherited.Authority)
	}
	if inherited.AckMode != session.AckOnDelivery {
		t.Fatalf("inherited AckMode = %q, want ack-on-delivery", inherited.AckMode)
	}
	if inherited.MaxUnacked != 0 {
		t.Fatalf("inherited MaxUnacked = %d, want 0", inherited.MaxUnacked)
	}
	if inherited.ID != spec.ID || inherited.Sink == nil ||
		inherited.Visibility != spec.Visibility ||
		inherited.QueueSize != spec.QueueSize ||
		inherited.DeliveryTimeout != spec.DeliveryTimeout {
		t.Fatalf("inherited spec dropped fields: %+v", inherited)
	}
}

func TestSyncDelegationInheritsAuthoritativeSinkAsObserver(t *testing.T) {
	const deltas = 40
	received := make(chan agent.StreamDeltaPayload, deltas*2)
	sink := agent.StreamSinkFunc(func(
		_ context.Context,
		_ event.Envelope,
		delta agent.StreamDeltaPayload,
	) error {
		received <- delta
		return nil
	})
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		for range deltas {
			if err := agent.EmitStreamPart(
				ctx, host, run.RunID, "writer.node.work",
				message.TextPart{Text: "p"}); err != nil {
				return nil, err
			}
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	service, _ := newSessionPathService(t, engine, nil)
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:         "authority",
			Sink:       sink,
			QueueSize:  64,
			Visibility: session.VisibilityConfirmed,
			Authority:  session.AuthorityAuthoritative,
			AckMode:    session.AckExplicit,
			MaxUnacked: 10,
		}},
		Inheritable: true,
	})
	response, err := service.Delegate(ctx, syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	// The spec carries an explicit MaxUnacked window of 10 with no ack
	// source: an un-downgraded authoritative attachment would be detached
	// with BudgetExceeded after ~10 deliveries, dropping the tail.
	// Observer inheritance must deliver every delta. The queue (64) is
	// sized so queue-full backpressure cannot mask the distinction.
	deadline := time.After(5 * time.Second)
	for i := 0; i < deltas; i++ {
		select {
		case delta := <-received:
			if part, ok := delta.Part.(message.TextPart); !ok || part.Text != "p" {
				t.Fatalf("delta part = %#v, want text %q", delta.Part, "p")
			}
		case <-deadline:
			t.Fatalf("inherited sink received %d of %d subagent deltas", i, deltas)
		}
	}
}

func TestSyncDelegationPropagatesStreamSinksTransitively(t *testing.T) {
	received := make(chan agent.StreamDeltaPayload, 8)
	sink := agent.StreamSinkFunc(func(
		_ context.Context,
		_ event.Envelope,
		delta agent.StreamDeltaPayload,
	) error {
		received <- delta
		return nil
	})
	var service *LocalService
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		switch run.AgentID {
		case "writer":
			// The writer is itself a delegated subagent; delegating from
			// inside its engine exercises the full context chain instead
			// of stamping the policy on the caller context directly.
			response, err := service.Delegate(ctx, syncRequest("researcher"))
			if err != nil {
				return board, err
			}
			if response.Status != StatusSucceeded {
				return board, errdefs.Internalf(
					"nested delegate status = %q, want succeeded", response.Status)
			}
			board.AppendChannelMessage(agent.MainChannel,
				message.NewTextMessage(message.RoleAssistant, "writer-done"))
			return board, nil
		case "researcher":
			if err := agent.EmitStreamPart(
				ctx, host, run.RunID, "researcher.node.work",
				message.TextPart{Text: "grandchild"}); err != nil {
				return nil, err
			}
			board.AppendChannelMessage(agent.MainChannel,
				message.NewTextMessage(message.RoleAssistant, "researcher-done"))
			return board, nil
		default:
			return board, errdefs.Internalf("unexpected agent %q", run.AgentID)
		}
	})
	service, _ = newSessionPathService(t, engine, nil)
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:   "caller",
			Sink: sink,
		}},
		Inheritable: true,
	})
	response, err := service.Delegate(ctx, syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	awaitStreamText(t, received, "grandchild")
}

// resumeStreamEngine fails the first attempt after writing a checkpoint
// and, on the resumed attempt, emits one stream delta before completing.
type resumeStreamEngine struct {
	mu       sync.Mutex
	attempts int
	store    *delegationCheckpointStore
}

func (e *resumeStreamEngine) Execute(
	ctx context.Context,
	run agent.Run,
	host agent.Host,
	board *agent.Board,
) (*agent.Board, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempts++
	if e.attempts == 1 {
		if err := e.store.Save(ctx, agent.Checkpoint{
			ExecID:            run.RunID,
			Steps:             []string{"wave-1"},
			Board:             board.Snapshot(),
			Timestamp:         time.Now(),
			OriginalStartedAt: time.Now(),
			SpecVersion:       "v1",
		}); err != nil {
			return board, err
		}
		return board, errdefs.Internalf("first attempt fails")
	}
	if err := agent.EmitStreamPart(
		ctx, host, run.RunID, "writer.node.work",
		message.TextPart{Text: "resumed"}); err != nil {
		return board, err
	}
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleAssistant, "done"))
	return board, nil
}

func (*resumeStreamEngine) CanResume(agent.Checkpoint) error { return nil }

func TestResumeInheritsStreamSinks(t *testing.T) {
	store := newDelegationCheckpointStore()
	engine := &resumeStreamEngine{store: store}
	service, _ := newSessionPathService(t, engine,
		&testSessionProvider{contextID: "resume-sink-ctx", persistent: true},
		session.WithResume(true), session.WithCheckpointStore(store))
	received := make(chan agent.StreamDeltaPayload, 8)
	sink := agent.StreamSinkFunc(func(
		_ context.Context,
		_ event.Envelope,
		delta agent.StreamDeltaPayload,
	) error {
		received <- delta
		return nil
	})
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:   "caller",
			Sink: sink,
		}},
		Inheritable: true,
	})
	first, err := service.Delegate(ctx, syncRequest("writer"))
	if err != nil || first.Status != StatusFailed {
		t.Fatalf("first = (%+v, %v), want failed", first, err)
	}
	second, err := service.Delegate(ctx, syncRequest("writer"))
	if err != nil {
		t.Fatalf("retry Delegate: %v", err)
	}
	if second.Status != StatusSucceeded {
		t.Fatalf("retry status = %q, want succeeded", second.Status)
	}
	awaitStreamText(t, received, "resumed")
}
