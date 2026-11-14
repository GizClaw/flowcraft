package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// failingStore injects one failure while delegating the rest.
type failingStore struct {
	agent.Store
	loadErr   error
	latestErr error
	saveErr   error
}

func (s *failingStore) Load(ctx context.Context, taskID string) (agent.Checkpoint, error) {
	if s.loadErr != nil {
		return agent.Checkpoint{}, s.loadErr
	}
	return s.Store.Load(ctx, taskID)
}

func (s *failingStore) LatestForSession(ctx context.Context, sessionID string) (agent.Checkpoint, error) {
	if s.latestErr != nil {
		return agent.Checkpoint{}, s.latestErr
	}
	return s.Store.LatestForSession(ctx, sessionID)
}

func (s *failingStore) Save(ctx context.Context, checkpoint agent.Checkpoint, events []agent.EventRecord, _ ...agent.SaveOption) (int64, int64, error) {
	if s.saveErr != nil {
		return 0, 0, s.saveErr
	}
	return s.Store.Save(ctx, checkpoint, events)
}

// State answers from the store for a session this process never opened.
func TestAgentStateReadsTheStore(t *testing.T) {
	store := agenttest.NewMemStore()
	ctx := context.Background()
	if _, _, err := store.Save(ctx, agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:one",
		Task:        agent.Task{ID: "t1", SessionID: "c1", State: agent.StateInputRequired},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{Store: store, Identity: "sha256:one", NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	state, err := a.State(ctx, "c1")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != agent.StateInputRequired {
		t.Errorf("state = %s, want input-required", state)
	}
	if len(*built) != 0 {
		t.Errorf("State built a runner; a lookup must stay cheap")
	}

	// The same store under another deployment is not this deployment's state.
	other, err := agent.NewAgent(agent.AgentOptions{
		Store: store, Identity: "sha256:two", NewRunner: factory,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if state, _ := other.State(ctx, "c1"); state != agent.StateSubmitted {
		t.Errorf("foreign state = %s, want submitted", state)
	}
}

// Store failures reach the caller instead of being reported as a fresh task.
func TestAgentPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("store is down")
	store := &failingStore{Store: agenttest.NewMemStore(), latestErr: boom}
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{Store: store, NewRunner: factory})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	if err := firstError(a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi"))); !errors.Is(err, boom) {
		t.Errorf("Run = %v, want the store error", err)
	}
	if _, err := a.State(ctx, "c1"); !errors.Is(err, boom) {
		t.Errorf("State = %v, want the store error", err)
	}
	if err := a.Cancel(ctx, "c1", "nobody"); !errors.Is(err, boom) {
		t.Errorf("Cancel = %v, want the store error", err)
	}
}

// Closing a task from another deployment is best effort: a failed write must
// not stop the session from continuing on this deployment.
func TestAgentAbandonsWithoutFailingTheRun(t *testing.T) {
	boom := errors.New("write refused")
	store := &failingStore{Store: agenttest.NewMemStore(), saveErr: boom}
	ctx := context.Background()
	if _, _, err := store.Store.Save(ctx, agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: "sha256:one",
		Task:        agent.Task{ID: "t1", SessionID: "c1", State: agent.StateInputRequired},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	factory, _ := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{
		Store: store, Identity: "sha256:two", NewRunner: factory,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	events := drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "hi")))
	if len(events) != 3 {
		t.Fatalf("events = %d, want the new task to run anyway", len(events))
	}
}
