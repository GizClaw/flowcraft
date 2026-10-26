package a2a

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestExecutorCheckpointsIncrementally pins that a task boundary goes through
// the shared writer: the first transition stores a base and the next ones extend
// it, instead of re-encoding the whole board each time.
func TestExecutorCheckpointsIncrementally(t *testing.T) {
	const runID = "run-checkpoint"
	store := agenttest.NewJournalStore()
	host := agenttest.StoreHost{Store: store}
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "start"))

	x := &executor{
		run:         agent.Run{Identity: agent.Identity{AgentID: "agent-a", RunID: runID}},
		host:        host,
		board:       board,
		taskID:      "task-1",
		seen:        map[string]bool{},
		checkpoints: agent.NewCheckpointWriter(host, runID),
	}

	x.checkpoint(context.Background())
	if got := store.BaseCount(); got != 1 {
		t.Fatalf("bases = %d, want 1 after the first boundary", got)
	}

	board.SetVar("observed", "working")
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "progress"))
	x.checkpoint(context.Background())

	if got := store.BaseCount(); got != 1 {
		t.Fatalf("bases = %d, want the second boundary to extend the first", got)
	}
	deltas := store.Deltas(runID)
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want 1", len(deltas))
	}
	if _, ok := deltas[0].Board.Vars()["observed"]; !ok {
		t.Fatal("the delta does not carry the boundary's var write")
	}
	cp, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cp == nil || len(cp.Board.Channels[agent.MainChannel]) != 2 {
		t.Fatalf("rebuilt record = %+v, want both messages", cp)
	}
	if got, want := cp.Payload, len(cp.Payload); got == nil || want == 0 {
		t.Fatal("the record lost the engine payload that carries the resume state")
	}
}
