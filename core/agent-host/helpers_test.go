package host

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/goccy/go-json"
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

// textRunner answers once with text and remembers the state it announced.
type textRunner struct {
	text  string
	mu    sync.Mutex
	state agent.TaskState
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

// a2aKind stands in for the A2A registration: these tests exercise the
// manager's bookkeeping, not the protocol's translation.
func a2aKind() deploy.ProtocolKind {
	return deploy.ProtocolKind{
		Name: "a2a",
		Decode: func(json.RawMessage) (any, error) {
			return nil, nil
		},
		Build: func(context.Context, deploy.ProtocolDeps) (deploy.Protocol, error) {
			return stubProtocol{}, nil
		},
	}
}

type stubProtocol struct{}

func (stubProtocol) Handler() http.Handler { return http.NewServeMux() }
func (stubProtocol) Close() error          { return nil }

// testRoots is the runtime root of a test, keyed by the test itself so the
// manager and the spec agree on one directory.
var (
	testRootsMu sync.Mutex
	testRoots   = map[*testing.T]string{}
)

// rootFor returns this test's runtime root, creating it on first use.
//
// The manager calls it before registering its own cleanup, so the temp directory
// is registered first and removed last: cleanup runs in reverse registration
// order, and Windows refuses to unlink a directory whose handle is still open.
func rootFor(t *testing.T) string {
	t.Helper()
	testRootsMu.Lock()
	defer testRootsMu.Unlock()
	if root, ok := testRoots[t]; ok {
		return root
	}
	root := t.TempDir()
	testRoots[t] = root
	return root
}

func config(t *testing.T) runtime.Config {
	t.Helper()
	root := rootFor(t)
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
func engineFor(name string, runner func() agent.Runner) deploy.EngineKind {
	return deploy.EngineKind{
		Name:  name,
		Local: true,
		Build: func(context.Context, deploy.EngineDeps) (agent.RunnerFactory, error) {
			return func(context.Context, string, string) (agent.Runner, error) {
				return runner(), nil
			}, nil
		},
	}
}

// newManager builds a manager and closes it with the test.
func newManager(t *testing.T, kinds deploy.Kinds, bindings Bindings) *Manager {
	t.Helper()
	// Create the runtime root before this cleanup is registered (see rootFor).
	rootFor(t)
	manager := New(kinds, bindings)
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// testHost is the wiring every test needs: a scripted driver and nothing else.
func testHost(t *testing.T) Bindings {
	t.Helper()
	return Bindings{RuntimeOptions: []runtime.Option{driver(&scripted{})}}
}

// testSpec is one agent on a fresh runtime: the shape every manager test needs.
func testSpec(t *testing.T, name, kind string) deploy.Spec {
	t.Helper()
	return deploy.Spec{Name: name, Engine: deploy.EngineSpec{Kind: kind}, Runtime: config(t)}
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

// testLoader registers one local fake kind beside the built-in a2a engine.
func testLoader(t *testing.T) deploy.Loader {
	t.Helper()
	return deploy.Loader{
		Kinds: deploy.BuiltinKinds().
			WithEngines(deploy.EngineKind{Name: "local", Local: true}).
			WithProtocols(a2aKind()),
	}
}
