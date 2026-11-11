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

	result, err := run.Result(context.Background())
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

	result, err := run.Result(context.Background())
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

func TestRunnerResultForwardsToTheRun(t *testing.T) {
	g := build(t, resultDoc, assistantKind{})
	runner := NewRunner(g, RunnerConfig{})
	for event, runErr := range runner.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
		_ = event
	}
	result, err := runner.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.Content.Text() != "the answer" {
		t.Fatalf("content = %q", result.Content.Text())
	}
}

func TestGraphResultConformance(t *testing.T) {
	g := build(t, resultDoc, assistantKind{})
	agenttest.Result(t, func(*testing.T) agent.Runner {
		return NewRunner(g, RunnerConfig{})
	})
}

func TestGraphSteerConformance(t *testing.T) {
	g := build(t, resultDoc, assistantKind{})
	agenttest.Steer(t, func(*testing.T) agent.Runner {
		return NewRunner(g, RunnerConfig{})
	})
}
