package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/server"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
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

// textRunner answers once with text. The A2A client only needs a peer that
// speaks A2A, not a local engine.
type textRunner struct {
	mu    sync.Mutex
	state agent.TaskState
	text  string
}

func (r *textRunner) State() agent.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "" {
		return agent.StateSubmitted
	}
	return r.state
}

func (r *textRunner) Run(_ context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.mu.Lock()
		r.state = agent.StateWorking
		r.mu.Unlock()
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		if !yield(agent.Text{Text: r.text}, nil) {
			return
		}
		r.mu.Lock()
		r.state = agent.StateCompleted
		r.mu.Unlock()
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (r *textRunner) Cancel(context.Context, string) error { return nil }
func (r *textRunner) Close() error                         { return nil }

// backend serves an A2A agent answering with text, for tests that need a peer.
func backend(t *testing.T, text string) *httptest.Server {
	t.Helper()
	srv, err := server.NewServer(server.ServerConfig{
		Card: server.CardConfig{Name: "backend", Version: "1.0.0", URL: "https://backend.test"},
		Runners: func(context.Context, *a2asrv.ExecutorContext) (agent.Runner, error) {
			return &textRunner{text: text}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
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

// An idle session starts its Run from Submit: a caller should not have to
// know whether it needs to pump the loop.

const runtimeLayer = `
version: 1
sandbox: {backend: local, writes: ["/work"]}
files: {roots: ["/work"]}
inference:
  providers: [{name: p, driver: stub}]
  models: [{provider: p, model: m}]
`

func writeLayer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(runtimeLayer), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	return path
}

// blockingRunner holds its run until released or cancelled.
type blockingRunner struct {
	mu        sync.Mutex
	state     agent.TaskState
	once      sync.Once
	started   chan struct{}
	release   chan struct{}
	cancelled atomic.Bool
}

func (r *blockingRunner) State() agent.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "" {
		return agent.StateSubmitted
	}
	return r.state
}

func (r *blockingRunner) Run(ctx context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.once.Do(func() { close(r.started) })
		r.mu.Lock()
		r.state = agent.StateWorking
		r.mu.Unlock()
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		select {
		case <-r.release:
			r.mu.Lock()
			r.state = agent.StateCompleted
			r.mu.Unlock()
			yield(agent.Status{State: agent.StateCompleted}, nil)
		case <-ctx.Done():
			r.cancelled.Store(true)
			r.mu.Lock()
			r.state = agent.StateCanceled
			r.mu.Unlock()
			yield(agent.Status{State: agent.StateCanceled}, nil)
		}
	}
}

func (r *blockingRunner) Cancel(context.Context, string) error { return nil }
func (r *blockingRunner) Close() error                         { return nil }

// completedRunner finishes on its first event.
type completedRunner struct {
	mu    sync.Mutex
	state agent.TaskState
}

func (r *completedRunner) State() agent.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "" {
		return agent.StateSubmitted
	}
	return r.state
}

func (r *completedRunner) Run(_ context.Context, _ message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		r.mu.Lock()
		r.state = agent.StateWorking
		r.mu.Unlock()
		if !yield(agent.Status{State: agent.StateWorking}, nil) {
			return
		}
		r.mu.Lock()
		r.state = agent.StateCompleted
		r.mu.Unlock()
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}
func (*completedRunner) Cancel(context.Context, string) error { return nil }
func (*completedRunner) Close() error                         { return nil }

// engineFor registers one local engine kind that hands out the given runner.
func engineFor(name string, runner func() agent.Runner) EngineKind {
	return EngineKind{
		Name:  name,
		Local: true,
		Build: func(context.Context, EngineDeps) (agent.RunnerFactory, error) {
			return func(context.Context, string, string) (agent.Runner, error) {
				return runner(), nil
			}, nil
		},
	}
}

// newManager builds a manager and closes it with the test.
func newManager(t *testing.T, kinds Kinds, host Host) *Manager {
	t.Helper()
	manager := New(kinds, host)
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// testHost is the wiring every test needs: a scripted driver and nothing else.
func testHost(t *testing.T) Host {
	t.Helper()
	return Host{RuntimeOptions: []runtime.Option{driver(&scripted{})}}
}

// testSpec is one agent on a fresh runtime: the shape every manager test needs.
func testSpec(t *testing.T, name, kind string) Spec {
	t.Helper()
	return Spec{Name: name, Engine: EngineSpec{Kind: kind}, Runtime: config(t)}
}

// rawJSON turns a Go literal into the JSON a settings block now is.
func rawJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

// rawSettings renders an engine settings block the way the document would.
func rawSettings(value any) json.RawMessage {
	return rawJSON(value)
}
