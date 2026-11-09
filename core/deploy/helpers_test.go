package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"testing"

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

// textRunner answers once with text. The proxy and the protocol adapter only need
// a peer that speaks A2A, not a local engine.
type textRunner struct{ text string }

func (r *textRunner) SetIdentity(string, string) {}
func (r *textRunner) State() agent.TaskState     { return agent.StateSubmitted }

func (r *textRunner) Run(_ context.Context, _ agent.Input) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		if !yield(agent.Text{Text: r.text}, nil) {
			return
		}
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (r *textRunner) Resume(ctx context.Context, _ agent.Reply) iter.Seq2[agent.Event, error] {
	return r.Run(ctx, agent.Input{})
}

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
		Build: func(json.RawMessage) (inference.Provider, error) {
			return inference.Provider{Generator: provider}, nil
		},
	})
}

// An idle conversation starts its Run from Submit: a caller should not have to
// know whether it needs to pump the loop.
