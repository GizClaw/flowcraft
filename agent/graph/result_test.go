package graph

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// assistantKind writes one assistant message, or suspends with a question.
type assistantKind struct{ suspend bool }

func (assistantKind) Name() string { return "assistant" }

func (a assistantKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	if a.suspend {
		return Static(Result{Suspend: &Suspend{
			Question: message.NewTextContent("which one?"),
		}}), nil
	}
	msg := message.NewTextMessage(message.RoleAssistant, "the answer")
	return Static(Result{State: []StateUpdate{{
		Key:   "messages",
		Value: MessagesValue(MessagesFromSlice([]message.Message{msg})),
	}}}), nil
}

const resultDoc = `
version: 1
entry: answer
state:
  messages: {type: messages}
nodes:
  - {id: answer, kind: assistant}
edges:
  - {from: answer, to: end}
`

func drainResult(t *testing.T, run *Run) {
	t.Helper()
	for event, err := range run.Events(context.Background()) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		_ = event
	}
}

func TestRunResultReturnsTheFinalAssistantMessage(t *testing.T) {
	g := build(t, resultDoc, assistantKind{})
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drainResult(t, run)

	result, err := run.RunSnapshot(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.State != agent.StateCompleted {
		t.Fatalf("state = %s, want completed", result.State)
	}
	if result.Content.Text() != "the answer" {
		t.Fatalf("content = %q", result.Content.Text())
	}
	if result.Question != nil {
		t.Fatalf("question = %+v, want nil", result.Question)
	}
}

func TestRunResultCarriesTheSuspendedQuestion(t *testing.T) {
	g := build(t, resultDoc, assistantKind{suspend: true})
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drainResult(t, run)

	result, err := run.RunSnapshot(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.State != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", result.State)
	}
	if result.Question == nil || result.Question.Text() != "which one?" {
		t.Fatalf("question = %+v", result.Question)
	}
}

// A driver keeps the run it opened, so a caller that wants the answer after the turn does not have to
// rebuild it from the transcript: the run is where the projection lives.
func TestDriverKeepsTheRunForItsSnapshot(t *testing.T) {
	g := build(t, resultDoc, assistantKind{})
	program, drv := openDriver(t, g, "conv-1")
	defer func() { _ = program.Close() }()
	defer func() { _ = drv.Close() }()

	if _, err := turn(t, drv, requestFor(agenttest.NewMemStore(), "task-1", "hi")); err != nil {
		t.Fatalf("run: %v", err)
	}
	d, ok := drv.(*driver)
	if !ok {
		t.Fatalf("driver = %T, want the graph's own", drv)
	}
	d.mu.Lock()
	run := d.run
	d.mu.Unlock()
	if run == nil {
		t.Fatal("the driver dropped the run it opened")
	}
	snapshot, err := run.RunSnapshot(context.Background())
	if err != nil {
		t.Fatalf("RunSnapshot: %v", err)
	}
	if snapshot.Content.Text() != "the answer" {
		t.Fatalf("content = %q", snapshot.Content.Text())
	}
}
