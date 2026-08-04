package graph

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/message"
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
	if msg.Role != message.RoleAssistant || msg.Content.Text() != "hello world" {
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

func TestMessageStream_MaterializeIsIdempotent(t *testing.T) {
	board := agent.NewBoard()
	ec := ExecutionContext{Context: context.Background(), Host: agent.NoopHost{}, NodeID: "n1"}

	s := ec.NewMessageStream("")
	if err := s.Emit("partial"); err != nil {
		t.Fatal(err)
	}
	first, err := s.Close(board)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Close(board)
	if err != nil {
		t.Fatal(err)
	}
	if first.Content.Text() != "partial" || second.Content.Text() != "partial" {
		t.Fatalf("repeated materialization returned %q then %q", first.Content.Text(), second.Content.Text())
	}
	if got := board.Channel(agent.MainChannel); len(got) != 1 {
		t.Fatalf("repeated materialization appended %d messages, want exactly 1", len(got))
	}

	empty := ec.NewMessageStream("empty")
	if _, err := empty.Close(board); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Close(board); err != nil {
		t.Fatal(err)
	}
	if got := board.Channel("empty"); len(got) != 0 {
		t.Fatalf("repeated empty materialization appended %d messages", len(got))
	}
}
