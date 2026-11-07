package deploy

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// scripted answers each round from a list.
type scripted struct {
	steps []inference.GenerateResponse
}

func (p *scripted) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	if len(p.steps) == 0 {
		return nil, errors.New("scripted provider ran out of steps")
	}
	step := p.steps[0]
	p.steps = p.steps[1:]
	// A real driver streams the answer as deltas; the loop forwards those, so a
	// scripted provider that only fills the response message would leave the
	// event stream empty.
	var events []inference.StreamEvent
	if text := step.Message.Content.Text(); text != "" {
		events = append(events, inference.StreamEvent{Kind: inference.StreamText, Text: text})
	}
	return &stream{events: events, resp: step}, nil
}

func (p *scripted) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	inner, err := p.GenerateStream(ctx, request)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	for {
		if _, err := inner.Next(ctx); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return inference.GenerateResponse{}, err
		}
	}
	return inner.Result()
}

type stream struct {
	events []inference.StreamEvent
	resp   inference.GenerateResponse
}

func (s *stream) Next(context.Context) (inference.StreamEvent, error) {
	if len(s.events) == 0 {
		return inference.StreamEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (s *stream) Result() (inference.GenerateResponse, error) { return s.resp, nil }
func (s *stream) Close() error                                { return nil }

func textStep(text string) inference.GenerateResponse {
	return inference.GenerateResponse{Message: message.NewTextMessage(message.RoleAssistant, text)}
}

func config(t *testing.T) runtime.Config {
	t.Helper()
	root := t.TempDir()
	return runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Inference: runtime.InferenceConfig{
			Providers: []runtime.ProviderConfig{{Name: "p", Driver: "stub"}},
			Models:    []runtime.ModelConfig{{Provider: "p", Model: "m"}},
		},
		Tools: runtime.ToolsConfig{Enable: []string{"files"}},
	}
}

func driver(provider inference.Generator) runtime.Option {
	return runtime.WithDrivers(runtime.Driver{
		Name: "stub",
		Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: provider}, nil
		},
	})
}

// waiting collects the states the loop reports.
func waiting() (chan agent.TaskState, func(string, agent.Event)) {
	states := make(chan agent.TaskState, 16)
	return states, func(_ string, event agent.Event) {
		if status, ok := event.(agent.Status); ok {
			states <- status.State
		}
	}
}

func waitFor(t *testing.T, states chan agent.TaskState, want agent.TaskState) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case state := <-states:
			if state == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

// An idle conversation starts its Run from Submit: a caller should not have to
// know whether it needs to pump the loop.
func TestSubmitRunsToCompletion(t *testing.T) {
	states, onEvent := waiting()
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{steps: []inference.GenerateResponse{textStep("hi")}})},
		OnEvent:        onEvent,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deployed, _ := manager.Agent("a")
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "hello"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, states, agent.StateCompleted)
}

// A paused Task resumes through the same surface, not through a raw Session.
func TestResumeAnswersAPausedTask(t *testing.T) {
	states, onEvent := waiting()
	call, err := message.NewToolCall("call-1", "ask_user", map[string]any{"question": "who?"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	provider := &scripted{steps: []inference.GenerateResponse{
		{Message: message.Message{Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}}}},
		textStep("done"),
	}}
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(provider)},
		OnEvent:        onEvent,
		Options: []agent.Option{agent.WithAsk(
			func(context.Context, agent.Prompt) (agent.Reply, error) { return agent.Reply{}, agent.ErrPaused })},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deployed, _ := manager.Agent("a")
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "who?"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, states, agent.StateInputRequired)
	if err := deployed.Resume(context.Background(), "conv-1", agent.Reply{Text: "me"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, states, agent.StateCompleted)
}

// Removing an agent stops it; further submissions are refused rather than
// silently queued.
func TestRemoveStopsTheAgent(t *testing.T) {
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{})},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deployed, _ := manager.Agent("a")
	if err := manager.Remove(context.Background(), "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, found := manager.Agent("a"); found {
		t.Error("the agent is still registered")
	}
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "hello"), agent.ModeQueue); err == nil {
		t.Error("a closed agent accepted a submission")
	}
}

func TestAddRejectsDuplicatesAndEmptyConfigs(t *testing.T) {
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	spec := Spec{Name: "a", Engine: EngineSpec{Kind: "react"},
		Runtime: config(t), RuntimeOptions: []runtime.Option{driver(&scripted{})}}
	if _, err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := manager.Add(context.Background(), spec); err == nil {
		t.Error("duplicate name accepted")
	}
	if _, err := manager.Add(context.Background(), Spec{Name: "b"}); err == nil {
		t.Error("empty runtime config accepted")
	}
}

// pausedProvider returns one ask_user call first, then a text answer per round.
func pausedProvider(t *testing.T, after ...string) *scripted {
	t.Helper()
	call, err := message.NewToolCall("ask-1", "ask_user", map[string]any{"question": "who?"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	steps := []inference.GenerateResponse{{
		Message: message.Message{Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}}},
	}}
	for _, text := range after {
		steps = append(steps, textStep(text))
	}
	return &scripted{steps: steps}
}

func pausedAgent(t *testing.T, provider inference.Generator, onEvent func(string, agent.Event)) *Manager {
	t.Helper()
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(provider)},
		OnEvent:        onEvent,
		Options: []agent.Option{agent.WithAsk(
			func(context.Context, agent.Prompt) (agent.Reply, error) { return agent.Reply{}, agent.ErrPaused })},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return manager
}

// A steer while the Task waits on a question is the answer to it.
func TestSteerWhilePausedAnswersTheQuestion(t *testing.T) {
	states, onEvent := waiting()
	manager := pausedAgent(t, pausedProvider(t, "answered"), onEvent)
	deployed, _ := manager.Agent("a")
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "who?"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, states, agent.StateInputRequired)
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "me"), agent.ModeSteer); err != nil {
		t.Fatalf("Submit steer: %v", err)
	}
	waitFor(t, states, agent.StateCompleted)
}

// An interrupt while paused gives up on the question, and its message becomes
// the next Task: the conversation does not stall on an answer nobody will give.
func TestInterruptWhilePausedStartsANewTask(t *testing.T) {
	states, onEvent := waiting()
	manager := pausedAgent(t, pausedProvider(t, "fresh start"), onEvent)
	deployed, _ := manager.Agent("a")
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "who?"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, states, agent.StateInputRequired)
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "never mind, do this instead"), agent.ModeInterrupt); err != nil {
		t.Fatalf("Submit interrupt: %v", err)
	}
	waitFor(t, states, agent.StateCompleted)
}

// A queued message survives a pause: it is delivered as the next Task once the
// current one is answered, which is what makes it different from a steer.
func TestQueueWhilePausedWaitsForTheNextTask(t *testing.T) {
	states, onEvent := waiting()
	manager := pausedAgent(t, pausedProvider(t, "answered", "next task"), onEvent)
	deployed, _ := manager.Agent("a")
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "who?"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, states, agent.StateInputRequired)
	if err := deployed.Submit(context.Background(), "conv-1",
		message.NewTextMessage(message.RoleUser, "later"), agent.ModeQueue); err != nil {
		t.Fatalf("Submit queued: %v", err)
	}
	if err := deployed.Resume(context.Background(), "conv-1", agent.Reply{Text: "me"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, states, agent.StateCompleted)
	// The queued message becomes its own Task, so a second completion follows.
	waitFor(t, states, agent.StateCompleted)
}
