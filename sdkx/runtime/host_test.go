package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

func TestBaseHostCheckpointPersists(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	store := &recordingCheckpointStore{}
	factory, err := newBaseHostFactory(bus, store)
	if err != nil {
		t.Fatal(err)
	}
	host := mustBaseHost(t, factory)

	cp := agent.Checkpoint{
		ExecID: "run-1",
		Steps:  []string{"wave-1"},
		Board:  agent.NewBoard().Snapshot(),
	}
	if err := host.Checkpoint(context.Background(), cp); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if store.saved == nil || store.saved.ExecID != "run-1" || len(store.saved.Steps) != 1 {
		t.Fatalf("store.saved = %+v, want run-1 checkpoint", store.saved)
	}
}

func TestBaseHostCheckpointDropsWithoutStore(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()
	factory, err := newBaseHostFactory(bus)
	if err != nil {
		t.Fatal(err)
	}
	host := mustBaseHost(t, factory)
	if err := host.Checkpoint(context.Background(), agent.Checkpoint{
		ExecID: "run-1",
		Board:  agent.NewBoard().Snapshot(),
	}); err != nil {
		t.Fatalf("Checkpoint without store = %v, want nil", err)
	}
}

func mustBaseHost(t *testing.T, factory session.HostFactory) agent.Host {
	t.Helper()
	host, err := factory.NewHost(context.Background(), session.HostRequest{
		Key:        session.Key{AgentID: "bot", ContextID: "ctx"},
		RunID:      "run-1",
		Interrupts: make(chan agent.Interrupt, 1),
		AskUser: func(context.Context, agent.UserPrompt) (agent.UserReply, error) {
			return agent.UserReply{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return host
}

type recordingCheckpointStore struct {
	mu    sync.Mutex
	saved *agent.Checkpoint
}

func (s *recordingCheckpointStore) Save(_ context.Context, cp agent.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := cp.Clone()
	s.saved = &clone
	return nil
}

func (s *recordingCheckpointStore) Load(_ context.Context, _ string) (*agent.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		return nil, nil
	}
	clone := s.saved.Clone()
	return &clone, nil
}

func (s *recordingCheckpointStore) Delete(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = nil
	return nil
}
