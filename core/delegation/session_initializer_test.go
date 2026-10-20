package delegation

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/tool"
)

// initializerRecorder records what a SessionInitializer observed and the
// order in which the delegation lifecycle reached each seam.
type initializerRecorder struct {
	mu        sync.Mutex
	order     []string
	keys      []session.Key
	requests  []AsyncRequest
	contexts  []string
	err       error
	engineRan int
}

func (r *initializerRecorder) initialize(_ context.Context, key session.Key, req AsyncRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, "initialize")
	r.keys = append(r.keys, key)
	r.requests = append(r.requests, req)
	return r.err
}

func (r *initializerRecorder) engineHook() agent.Engine {
	return agent.EngineFunc(func(
		_ context.Context,
		run agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		r.mu.Lock()
		r.order = append(r.order, "engine")
		r.contexts = append(r.contexts, run.ConversationID)
		r.engineRan++
		r.mu.Unlock()
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
}

func (r *initializerRecorder) snapshot() ([]string, []session.Key, []AsyncRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...),
		append([]session.Key(nil), r.keys...),
		append([]AsyncRequest(nil), r.requests...)
}

// engineContextIDs returns the ConversationID of every run the engine
// executed, in order.
func (r *initializerRecorder) engineContextIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.contexts...)
}

func TestSessionInitializerRunsBeforeStart(t *testing.T) {
	recorder := &initializerRecorder{}
	service, _ := newSessionPathServiceWithOptions(t, recorder.engineHook(),
		&testSessionProvider{contextID: "init-ctx", persistent: true},
		[]Option{WithSessionInitializer(recorder.initialize)})

	ctx := tool.WithCallID(context.Background(), "call-init")
	ctx = agent.WithRunInfo(ctx, agent.RunInfo{
		Identity: agent.Identity{AgentID: "caller", RunID: "run-caller"},
	})
	response, err := service.Delegate(ctx, syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}

	order, keys, requests := recorder.snapshot()
	if len(order) != 2 || order[0] != "initialize" || order[1] != "engine" {
		t.Fatalf("lifecycle order = %v, want [initialize engine]", order)
	}
	if keys[0].ContextID != "init-ctx" || keys[0].AgentID != "writer" {
		t.Fatalf("initializer key = %+v, want the subagent session identity", keys[0])
	}
	if got := response.Metadata["delegation.session_id"]; got != keys[0].ContextID {
		t.Fatalf("initializer key %q != subagent ContextID %q", keys[0].ContextID, got)
	}
	if requests[0].CallID != "call-init" {
		t.Fatalf("initializer CallID = %q, want the delegate tool call id", requests[0].CallID)
	}
	if requests[0].ParentRunID != "run-caller" || requests[0].Depth != 1 {
		t.Fatalf("initializer request lineage = %+v, want run-caller at depth 1", requests[0])
	}
}

func TestSessionInitializerRunsOnResume(t *testing.T) {
	store := newDelegationCheckpointStore()
	engine := &resumeAwareDelegationEngine{store: store, writeCheckpoint: true}
	recorder := &initializerRecorder{}
	service, _ := newSessionPathServiceWithOptions(t, engine,
		&testSessionProvider{contextID: "resume-init", persistent: true},
		[]Option{WithSessionInitializer(recorder.initialize)},
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

	_, keys, _ := recorder.snapshot()
	if len(keys) != 2 {
		t.Fatalf("initializer ran %d times, want once per attempt (resume included)", len(keys))
	}
	if keys[0].ContextID != keys[1].ContextID {
		t.Fatalf("initializer keys differ across attempts: %v", keys)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	if !engine.resumed {
		t.Fatal("the retry did not resume the parked run")
	}
}

func TestSessionInitializerErrorFailsDelegation(t *testing.T) {
	recorder := &initializerRecorder{err: errdefs.PolicyDeniedf("no policy for this target")}
	service, _ := newSessionPathServiceWithOptions(t, recorder.engineHook(),
		&testSessionProvider{contextID: "denied-ctx", persistent: true},
		[]Option{WithSessionInitializer(recorder.initialize)})

	_, err := service.Delegate(context.Background(), syncRequest("writer"))
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("Delegate = %v, want the initializer's policy-denied error", err)
	}
	if _, _, requests := recorder.snapshot(); len(requests) != 1 {
		t.Fatalf("initializer calls = %d, want 1", len(requests))
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.engineRan != 0 {
		t.Fatal("the subagent ran despite the initializer failing")
	}
}

// TestSessionInitializerRunsOnLegacyPath pins that an injected provider
// is enough to initialize: the manager-less path still mints an identity,
// so the hook runs before the run and sees the same session.
func TestSessionInitializerRunsOnLegacyPath(t *testing.T) {
	recorder := &initializerRecorder{}
	service, err := NewService(boundDirectory(t, recorder.engineHook()), nil,
		WithSessionInitializer(recorder.initialize),
		WithSessionProvider(&testSessionProvider{contextID: "legacy-init-ctx", persistent: true}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer func() { _ = service.Close() }()

	response, err := service.Delegate(context.Background(), syncRequest("writer"))
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	order, keys, _ := recorder.snapshot()
	if len(order) != 2 || order[0] != "initialize" || order[1] != "engine" {
		t.Fatalf("lifecycle order = %v, want [initialize engine]", order)
	}
	if keys[0].ContextID != "legacy-init-ctx" || keys[0].AgentID != "writer" {
		t.Fatalf("initializer key = %+v, want the provider-minted identity", keys[0])
	}
	if ids := recorder.engineContextIDs(); len(ids) != 1 || ids[0] != keys[0].ContextID {
		t.Fatalf("run ContextIDs = %v, want the session the hook initialized (%q)",
			ids, keys[0].ContextID)
	}
}

// TestSessionInitializerLegacyErrorFailsDelegation pins the fail-closed
// contract on the manager-less path: the hook has the same veto it has on
// the session path.
func TestSessionInitializerLegacyErrorFailsDelegation(t *testing.T) {
	recorder := &initializerRecorder{err: errdefs.PolicyDeniedf("no policy for this target")}
	service, err := NewService(boundDirectory(t, recorder.engineHook()), nil,
		WithSessionInitializer(recorder.initialize),
		WithSessionProvider(&testSessionProvider{contextID: "legacy-denied", persistent: true}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer func() { _ = service.Close() }()

	_, err = service.Delegate(context.Background(), syncRequest("writer"))
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("Delegate = %v, want the initializer's policy-denied error", err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.engineRan != 0 {
		t.Fatal("the subagent ran despite the initializer failing")
	}
}

// TestSessionInitializerWithoutIdentityFailsDelegation pins the other half
// of the same contract: with neither a session manager nor a provider there
// is nothing to initialize, so a configured hook refuses the delegation
// instead of letting it run under the store's default.
func TestSessionInitializerWithoutIdentityFailsDelegation(t *testing.T) {
	recorder := &initializerRecorder{}
	service, err := NewService(boundDirectory(t, recorder.engineHook()), nil,
		WithSessionInitializer(recorder.initialize))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer func() { _ = service.Close() }()

	_, err = service.Delegate(context.Background(), syncRequest("writer"))
	if !errdefs.IsValidation(err) {
		t.Fatalf("Delegate = %v, want validation for a delegation without session identity", err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.engineRan != 0 {
		t.Fatal("the subagent ran without the session identity the hook was asked to initialize")
	}
}

func TestSessionInitializerNilRejected(t *testing.T) {
	_, err := NewService(NewDirectory(), nil, WithSessionInitializer(nil))
	if !errdefs.IsValidation(err) {
		t.Fatalf("WithSessionInitializer(nil) = %v, want validation", err)
	}
}
