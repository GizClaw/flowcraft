package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// An idle session is dropped from memory: its state lives in the store, so
// eviction is a cache decision.
func TestAgentEvictsByIdleTimeout(t *testing.T) {
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory, IdleTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "one")))
	time.Sleep(40 * time.Millisecond)
	drain(t, a.Run(ctx, "c2", message.NewTextMessage(message.RoleUser, "two")))
	if runner := (*built)[0]; runner.closeCalls != 1 {
		t.Errorf("idle runner closed %d times, want 1", runner.closeCalls)
	}
	if state, _ := a.State(ctx, "c1"); state != agent.StateSubmitted {
		t.Errorf("c1 state = %s, want the memory entry gone", state)
	}
}

// A running session is never evicted, even when the table is over its
// bound: it is not reconstructible while the runner is mid-step.
func TestAgentKeepsARunningSessionOverTheBound(t *testing.T) {
	var slow *fakeRunner
	factory := func(_ context.Context, sessionID, _ string) (agent.Runner, error) {
		runner := &fakeRunner{state: agent.StateSubmitted, block: sessionID == "c1"}
		if sessionID == "c1" {
			slow = runner
		}
		return runner, nil
	}
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory, MaxSessions: 1})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event, err := range a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "one")) {
			if err != nil {
				return
			}
			if status, ok := event.(agent.Status); ok && status.State == agent.StateWorking {
				close(started)
			}
		}
	}()
	<-started
	drain(t, a.Run(ctx, "c2", message.NewTextMessage(message.RoleUser, "two")))
	if slow.closeCalls != 0 {
		t.Errorf("the running session was evicted")
	}
	if state, _ := a.State(ctx, "c1"); state != agent.StateWorking {
		t.Errorf("c1 state = %s, want working", state)
	}
	if err := a.Cancel(ctx, "c1", "test over"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	<-done
}
