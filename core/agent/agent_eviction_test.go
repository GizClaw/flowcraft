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
//
// The eviction happens on the background sweep, not on the next run: the table is
// a cache, and looking at all of it on every message made each run pay for
// sessions nothing had asked about. So the assertion is a deadline, not a
// sequencing.
func TestAgentEvictsByIdleTimeout(t *testing.T) {
	factory, built := newFakeFactory(false)
	a, err := agent.NewAgent(agent.AgentOptions{NewRunner: factory, IdleTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx := context.Background()
	drain(t, a.Run(ctx, "c1", message.NewTextMessage(message.RoleUser, "one")))

	deadline := time.Now().Add(2 * time.Second)
	for (*built)[0].closes() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the idle runner was never closed")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if runner := (*built)[0]; runner.closes() != 1 {
		t.Errorf("idle runner closed %d times, want 1", runner.closes())
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
	if slow.closes() != 0 {
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
