package manage

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// blockedProvider holds every stream until released, so Drain has something real
// to wait for.
type blockedProvider struct {
	release chan struct{}
	once    sync.Once
}

func (p *blockedProvider) GenerateStream(ctx context.Context, _ inference.GenerateRequest) (inference.Stream, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &stream{resp: textStep("done")}, nil
}

func (p *blockedProvider) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	inner, err := p.GenerateStream(ctx, request)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	for {
		if _, err := inner.Next(ctx); errors.Is(err, io.EOF) {
			break
		}
	}
	return inner.Result()
}

func (p *blockedProvider) unblock() { p.once.Do(func() { close(p.release) }) }

// Drain stops new work, waits for the conversations in flight, and reports what
// is still running when the deadline passes.
func TestDrainWaitsAndRefusesNewWork(t *testing.T) {
	provider := &blockedProvider{release: make(chan struct{})}
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(provider)},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "hi"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	drained := make(chan error, 1)
	go func() { drained <- manager.Drain(context.Background()) }()
	time.Sleep(20 * time.Millisecond)

	if err := deployed.Submit(context.Background(), "conv-2",
		message.NewTextMessage(message.RoleUser, "hi"), agent.ModeQueue); err == nil {
		t.Error("a draining agent accepted new work")
	}
	provider.unblock()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not finish after the work did")
	}
}

func TestDrainReportsInFlightConversations(t *testing.T) {
	provider := &blockedProvider{release: make(chan struct{})}
	t.Cleanup(provider.unblock)
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(provider)},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "hi"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := manager.Drain(ctx); err == nil || !strings.Contains(err.Error(), "conv-1") {
		t.Fatalf("err = %v, want the in-flight conversation named", err)
	}
}

// Idle conversations leave memory; the next message restores them from the
// store, so eviction is a cache decision.
func TestIdleConversationIsEvicted(t *testing.T) {
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{})},
		IdleTimeout:    time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "hi"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	deployed.mu.Lock()
	deployed.sweepLocked(time.Now())
	remaining := len(deployed.sessions)
	deployed.mu.Unlock()
	if remaining != 0 {
		t.Errorf("sessions = %d, want the idle one evicted", remaining)
	}
}

// MaxSessions trims the oldest idle conversation, never a running one.
func TestMaxSessionsEvictsTheOldest(t *testing.T) {
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{})},
		MaxSessions:    1,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, id := range []string{"conv-1", "conv-2"} {
		if err := deployed.Submit(context.Background(), id,
			message.NewTextMessage(message.RoleUser, "hi"), agent.ModeQueue); err != nil {
			t.Fatalf("Submit %s: %v", id, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	deployed.mu.Lock()
	_, keptSecond := deployed.sessions["conv-2"]
	_, keptFirst := deployed.sessions["conv-1"]
	total := len(deployed.sessions)
	deployed.mu.Unlock()
	if total != 1 || !keptSecond || keptFirst {
		t.Errorf("sessions = %d (first=%v second=%v), want only the newest", total, keptFirst, keptSecond)
	}
}
