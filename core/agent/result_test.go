package agent_test

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

type resultRunner struct {
	state  agent.TaskState
	result agent.Result
}

func (r *resultRunner) State() agent.TaskState { return r.state }

func (r *resultRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.state = agent.StateCompleted
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (r *resultRunner) Cancel(context.Context, string) error { return nil }
func (r *resultRunner) Close() error                         { return nil }

func (r *resultRunner) Result(context.Context) (agent.Result, error) {
	return r.result, nil
}

func TestAgentResultReturnsTheFinalAnswer(t *testing.T) {
	question := message.NewTextContent("which one?")
	runner := &resultRunner{
		state: agent.StateSubmitted,
		result: agent.Result{
			State:     agent.StateCompleted,
			Content:   message.NewTextContent("the answer"),
			Artifacts: []agent.Artifact{{ID: "a-1", Blob: "sha256:x"}},
			Question:  &question,
		},
	}
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer func() { _ = a.Close() }()

	for event, runErr := range a.Run(context.Background(), "s-1",
		message.NewTextMessage(message.RoleUser, "hi")) {
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
		_ = event
	}

	result, err := a.Result(context.Background(), "s-1")
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.TaskID == "" {
		t.Error("task id is empty")
	}
	if result.State != agent.StateCompleted {
		t.Errorf("state = %s, want completed", result.State)
	}
	if result.Content.Text() != "the answer" {
		t.Errorf("content = %q", result.Content.Text())
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].ID != "a-1" {
		t.Errorf("artifacts = %+v", result.Artifacts)
	}
	if result.Question == nil || result.Question.Text() != "which one?" {
		t.Errorf("question = %+v", result.Question)
	}
}

type blockingResultRunner struct {
	started chan struct{}
	release chan struct{}
	state   agent.TaskState
}

func (r *blockingResultRunner) State() agent.TaskState { return r.state }

func (r *blockingResultRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.state = agent.StateWorking
		close(r.started)
		<-r.release
		r.state = agent.StateCompleted
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (r *blockingResultRunner) Cancel(context.Context, string) error { return nil }
func (r *blockingResultRunner) Close() error                         { return nil }
func (r *blockingResultRunner) Result(context.Context) (agent.Result, error) {
	return agent.Result{State: agent.StateCompleted}, nil
}

func TestAgentResultRefusesABusySession(t *testing.T) {
	runner := &blockingResultRunner{
		started: make(chan struct{}),
		release: make(chan struct{}),
		state:   agent.StateSubmitted,
	}
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer func() { _ = a.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range a.Run(context.Background(), "s-1",
			message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	<-runner.started
	if _, err := a.Result(context.Background(), "s-1"); !errors.Is(err, agent.ErrSessionBusy) {
		t.Fatalf("Result = %v, want ErrSessionBusy", err)
	}
	close(runner.release)
	<-done
}

type unsupportedRunner struct{ state agent.TaskState }

func (r *unsupportedRunner) State() agent.TaskState { return r.state }
func (r *unsupportedRunner) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.state = agent.StateCompleted
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}
func (r *unsupportedRunner) Cancel(context.Context, string) error { return nil }
func (r *unsupportedRunner) Close() error                         { return nil }

func TestAgentResultReportsAnUnsupportedEngine(t *testing.T) {
	runner := &unsupportedRunner{state: agent.StateSubmitted}
	a, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer func() { _ = a.Close() }()
	for range a.Run(context.Background(), "s-1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	if _, err := a.Result(context.Background(), "s-1"); !errors.Is(err, agent.ErrResultUnsupported) {
		t.Fatalf("Result = %v, want ErrResultUnsupported", err)
	}
}
