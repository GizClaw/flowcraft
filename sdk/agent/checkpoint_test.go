package agent_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
)

func TestNoopCheckpointStore_SaveLoad(t *testing.T) {
	var s agent.NoopCheckpointStore

	cp := agent.Checkpoint{
		ExecID:    "run-1",
		Steps:     []string{"node-2"},
		Iteration: 3,
		Board:     agent.NewBoard().Snapshot(),
		Timestamp: time.Now(),
	}
	if err := s.Save(context.Background(), cp); err != nil {
		t.Errorf("Save returned error: %v", err)
	}

	got, err := s.Load(context.Background(), "run-1")
	if err != nil {
		t.Errorf("Load returned error: %v", err)
	}
	if got != nil {
		t.Errorf("Noop Load must return nil, nil; got %+v", got)
	}
}

func TestCheckpoint_StoreInterfaces(t *testing.T) {
	// Compile-time assertion that NoopCheckpointStore satisfies the
	// CheckpointStore contract (it does not implement the optional
	// CheckpointLister / CheckpointDeleter interfaces).
	var _ agent.CheckpointStore = agent.NoopCheckpointStore{}
}

func TestCheckpoint_Validate(t *testing.T) {
	valid := agent.Checkpoint{
		ExecID: "run-1",
		Board:  agent.NewBoard().Snapshot(),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}

	cases := []struct {
		name string
		cp   agent.Checkpoint
	}{
		{"missing exec id", agent.Checkpoint{Board: agent.NewBoard().Snapshot()}},
		{"missing board", agent.Checkpoint{ExecID: "run-1"}},
		{"invalid payload", agent.Checkpoint{ExecID: "run-1", Board: agent.NewBoard().Snapshot(), Payload: []byte("{")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cp.Validate(); !errdefs.IsValidation(err) {
				t.Fatalf("Validate() = %v, want Validation", err)
			}
		})
	}

	t.Run("empty steps are valid", func(t *testing.T) {
		cp := agent.Checkpoint{ExecID: "run-1", Board: agent.NewBoard().Snapshot()}
		if err := cp.Validate(); err != nil {
			t.Fatalf("empty Steps must be allowed: %v", err)
		}
	})
}

func TestCheckpoint_Clone(t *testing.T) {
	cp := agent.Checkpoint{
		ExecID:    "run-1",
		Steps:     []string{"wave-1"},
		Iteration: 2,
		Board: &agent.BoardSnapshot{
			Vars:     map[string]any{"x": float64(1)},
			Channels: map[string][]message.Message{agent.MainChannel: {message.NewTextMessage(message.RoleAssistant, "hi")}},
		},
		Payload:    []byte(`{"task_id":"t1"}`),
		Attributes: map[string]string{"tenant": "tenant-a"},
	}

	clone := cp.Clone()

	cp.Steps[0] = "mutated"
	cp.Board.Vars["x"] = float64(99)
	cp.Board.Channels[agent.MainChannel][0].Content.Parts = nil
	cp.Payload[0] = 'x'
	cp.Attributes["tenant"] = "mutated"

	if clone.Steps[0] != "wave-1" {
		t.Errorf("Steps aliased: %v", clone.Steps)
	}
	if clone.Board.Vars["x"] != float64(1) {
		t.Errorf("board vars aliased: %v", clone.Board.Vars)
	}
	if len(clone.Board.Channels[agent.MainChannel]) != 1 ||
		clone.Board.Channels[agent.MainChannel][0].Content.Parts == nil {
		t.Errorf("board channels aliased: %v", clone.Board.Channels)
	}
	if string(clone.Payload) != `{"task_id":"t1"}` {
		t.Errorf("payload aliased: %q", clone.Payload)
	}
	if clone.Attributes["tenant"] != "tenant-a" {
		t.Errorf("attributes aliased: %v", clone.Attributes)
	}
}

func TestCheckpointStore_Conformance(t *testing.T) {
	agenttest.CheckpointStoreSuite(t, func() agent.CheckpointStore {
		return newConformanceMemStore()
	})
}

type conformanceMemStore struct {
	mu  sync.Mutex
	cps map[string]agent.Checkpoint
}

func newConformanceMemStore() *conformanceMemStore {
	return &conformanceMemStore{cps: make(map[string]agent.Checkpoint)}
}

func (s *conformanceMemStore) Save(_ context.Context, cp agent.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cps[cp.ExecID] = cp.Clone()
	return nil
}

func (s *conformanceMemStore) Load(_ context.Context, execID string) (*agent.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.cps[execID]
	if !ok {
		return nil, nil
	}
	clone := cp.Clone()
	return &clone, nil
}

func (s *conformanceMemStore) List(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.cps))
	for id := range s.cps {
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *conformanceMemStore) Delete(_ context.Context, execID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cps, execID)
	return nil
}
