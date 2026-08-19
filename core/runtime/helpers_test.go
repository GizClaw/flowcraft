package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/tool"
)

const (
	testEngineKind = "agent.Engine"
	testEngineImpl = "test"
	testEventKind  = "event.Bus"
	testEventImpl  = "test"
	testStoreKind  = "agent.CheckpointStore"
	testStoreImpl  = "test"
)

// withRunEnd wraps an engine so it publishes the engine contract's
// required run-end event after Execute returns. Stub engines built
// from agent.EngineFunc never publish it themselves, and without the
// event the session's stream coordinator waits out its full drain
// budget on every turn.
func withRunEnd(engine agent.Engine) agent.Engine {
	return agent.EngineFunc(func(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		board, err := engine.Execute(ctx, run, host, board)
		publishCtx := context.WithoutCancel(ctx)
		envelope, envelopeErr := event.NewEnvelope(publishCtx, agent.SubjectRunEnd(run.RunID), nil)
		if envelopeErr == nil {
			envelope.SetRunID(run.RunID)
			envelopeErr = host.Publish(publishCtx, envelope)
		}
		if err != nil {
			return board, err
		}
		return board, envelopeErr
	})
}

func noopEngine() agent.Engine {
	return withRunEnd(agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	}))
}

type testEngineFactory struct {
	engine agent.Engine
}

func (f testEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: testEngineKind, Impl: testEngineImpl}
}

func (f testEngineFactory) New(context.Context, resource.Input) (any, error) {
	if f.engine == nil {
		return nil, errors.New("test engine factory: nil engine")
	}
	return f.engine, nil
}

type testResourceFactory struct {
	spec  resource.Spec
	value any
	err   error
	calls *int
}

func (f testResourceFactory) Spec() resource.Spec { return f.spec }

func (f testResourceFactory) New(_ context.Context, _ resource.Input) (any, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.value, f.err
}

// newBaseRegistry registers the test engine plus the optional event
// bus and checkpoint store resources used by most runtime tests.
func newBaseRegistry(
	t *testing.T,
	bus any,
	store any,
	engine agent.Engine,
) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: engine})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: bus,
	})
	if store != nil {
		reg.MustRegister(testResourceFactory{
			spec:  resource.Spec{Kind: testStoreKind, Impl: testStoreImpl},
			value: store,
		})
	}
	return reg
}

func parseRuntimeDoc(t *testing.T, yaml string) deploy.Document {
	t.Helper()
	doc, err := deploy.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("deploy.Parse: %v", err)
	}
	return doc
}

func runtimeDoc(t *testing.T, extra string) deploy.Document {
	t.Helper()
	return parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: test}
  cps: {kind: agent.CheckpointStore, impl: test}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
  checkpoint_store: cps
  sessions: {idle_timeout: 1s, sink_buffer: 8}
`+extra)
}

func baseRuntimeDoc(t *testing.T) deploy.Document {
	t.Helper()
	return runtimeDoc(t, "")
}

type trackedBus struct {
	*event.MemoryBus
	mu       *sync.Mutex
	log      *[]string
	closeErr error
}

func (b *trackedBus) Close() error {
	b.mu.Lock()
	*b.log = append(*b.log, "bus.close")
	b.mu.Unlock()
	_ = b.MemoryBus.Close()
	return b.closeErr
}

type recordingCheckpointStore struct {
	mu    sync.Mutex
	saved *agent.Checkpoint
}

func (s *recordingCheckpointStore) Save(_ context.Context, cp agent.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := cp.Clone()
	s.saved = &clone
	return nil
}

func (s *recordingCheckpointStore) Load(_ context.Context, _ string) (*agent.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		return nil, nil
	}
	clone := s.saved.Clone()
	return &clone, nil
}

func (s *recordingCheckpointStore) Delete(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = nil
	return nil
}

func mustBaseHost(t *testing.T, factory session.HostFactory) agent.Host {
	t.Helper()
	host, err := factory.NewHost(context.Background(), session.HostRequest{
		Key:        session.Key{AgentID: "bot", ContextID: "ctx"},
		RunID:      "run-1",
		Interrupts: make(chan agent.Interrupt, 1),
		AskUser: func(context.Context, agent.UserPrompt) (agent.UserReply, error) {
			return agent.UserReply{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return host
}

// testSource contributes a fixed set of eager tools.
type testSource struct {
	tools []tool.Tool
}

func (s testSource) Tools() []tool.Tool         { return s.tools }
func (s testSource) LazyTools() []tool.LazyTool { return nil }

func funcTool(name, content string) tool.Tool {
	return tool.FuncTool(
		message.ToolDefinition{
			Name:        name,
			Description: name,
			InputSchema: []byte(`{"type":"object"}`),
		},
		func(context.Context, string) (string, error) { return content, nil },
	)
}

func indexOf(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func runTurn(t *testing.T, manager interface {
	Open(context.Context, session.Key) (*session.Lease, error)
}, agentID, contextID string) *agent.Result {
	t.Helper()
	lease, err := manager.Open(context.Background(), session.Key{
		AgentID: agentID, ContextID: contextID,
	})
	if err != nil {
		t.Fatalf("Open(%s): %v", agentID, err)
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Start(%s): %v", agentID, err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait(%s): %v", agentID, err)
	}
	return result
}
