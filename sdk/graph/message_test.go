package graph

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

func TestMessageStream(t *testing.T) {
	board := agent.NewBoard()
	ctx := agent.WithRunInfo(context.Background(),
		agent.RunInfo{Identity: agent.Identity{AgentID: "a", RunID: "r"}})
	ec := ExecutionContext{
		Context: ctx,
		Host:    agent.NoopHost{},
		NodeID:  "n1",
	}

	s := ec.NewMessageStream("")
	if s.Channel() != agent.MainChannel {
		t.Fatalf("default channel = %q", s.Channel())
	}
	if err := s.Emit("hello "); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit("world"); err != nil {
		t.Fatal(err)
	}
	msg, err := s.Close(board)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != inference.RoleAssistant || msg.Content.Text() != "hello world" {
		t.Fatalf("message = %+v", msg)
	}
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 1 || msgs[0].Content.Text() != "hello world" {
		t.Fatalf("channel = %+v", msgs)
	}

	// Empty close is a no-op.
	s2 := ec.NewMessageStream("other")
	if _, err := s2.Close(board); err != nil {
		t.Fatal(err)
	}
	if len(board.Channel("other")) != 0 {
		t.Fatal("empty stream appended a message")
	}
}
