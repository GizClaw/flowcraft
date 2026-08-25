package runtime

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestStreamExportRegistry_ConversationLifecycle(t *testing.T) {
	reg := NewStreamExportRegistry(nil)
	received := make(chan agent.StreamDeltaPayload, 4)
	inner := agent.StreamSinkFunc(func(
		_ context.Context,
		_ event.Envelope,
		delta agent.StreamDeltaPayload,
	) error {
		received <- delta
		return nil
	})

	reg.RegisterConversation("ctx-1", inner)
	sink, ok := reg.ConversationSink("ctx-1")
	if !ok {
		t.Fatal("ConversationSink missing after register")
	}
	if _, ok := sink.(conversationStreamSink); !ok {
		t.Fatalf("registered sink type = %T, want conversationStreamSink", sink)
	}

	// Exporter describes the registry's own sinks.
	target, ok := reg.Exporter(session.SinkSpec{ID: "conv", Sink: sink})
	if !ok || target.Kind != delegation.StreamTargetKindConversation || target.ID != "ctx-1" {
		t.Fatalf("Exporter = %+v, %v; want conversation target ctx-1", target, ok)
	}
	// Exporter ignores foreign sinks.
	if _, ok := reg.Exporter(session.SinkSpec{
		ID: "foreign", Sink: agent.StreamSinkFunc(func(
			context.Context, event.Envelope, agent.StreamDeltaPayload,
		) error {
			return nil
		}),
	}); ok {
		t.Fatal("Exporter described a foreign sink")
	}

	// Resolver returns the registered sink and it forwards deltas.
	resolved, err := reg.Resolver(context.Background(), target)
	if err != nil {
		t.Fatalf("Resolver: %v", err)
	}
	if err := resolved.OnDelta(context.Background(), event.Envelope{},
		agent.StreamDeltaPayload{Type: agent.StreamDeltaPart,
			Part: message.TextPart{Text: "x"}}); err != nil {
		t.Fatalf("OnDelta: %v", err)
	}
	select {
	case delta := <-received:
		if delta.Type != agent.StreamDeltaPart {
			t.Fatalf("delta = %+v", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("resolved sink did not forward delta to inner sink")
	}

	reg.UnregisterConversation("ctx-1")
	if _, ok := reg.ConversationSink("ctx-1"); ok {
		t.Fatal("ConversationSink still present after unregister")
	}
	if _, err := reg.Resolver(context.Background(), target); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Resolver after unregister error = %v, want not available", err)
	}
	reg.UnregisterConversation("ctx-1")    // idempotent
	reg.RegisterConversation("ctx-1", nil) // ignored
	if _, ok := reg.ConversationSink("ctx-1"); ok {
		t.Fatal("nil register attached a sink")
	}
}

func TestStreamExportRegistry_ResolverWhitelist(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	reg := NewStreamExportRegistry(map[string]event.Bus{"events": bus})

	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: "unknown", ID: "x",
	}); !errdefs.IsPolicyDenied(err) {
		t.Fatalf("unknown kind error = %v, want policy denied", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: delegation.StreamTargetKindConversation,
	}); !errdefs.IsValidation(err) {
		t.Fatalf("empty conversation id error = %v, want validation", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: delegation.StreamTargetKindConversation, ID: "nope",
	}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("unknown conversation error = %v, want not available", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: delegation.StreamTargetKindBus,
	}); !errdefs.IsValidation(err) {
		t.Fatalf("empty bus id error = %v, want validation", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: delegation.StreamTargetKindBus, ID: "missing-bus",
	}); !errdefs.IsValidation(err) {
		t.Fatalf("unknown bus error = %v, want validation", err)
	}
	if _, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: delegation.StreamTargetKindBus, ID: "events",
	}); err != nil {
		t.Fatalf("known bus error = %v", err)
	}
}

// TestStreamExportRegistry_ExporterAcceptsDecoratedProviders verifies
// the exporter recognizes sinks through the StreamTargetProvider
// interface, so a decorator wrapping the registry's conversation sink
// does not silently break cross-process streaming.
func TestStreamExportRegistry_ExporterAcceptsDecoratedProviders(t *testing.T) {
	reg := NewStreamExportRegistry(nil)
	inner := agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	})
	reg.RegisterConversation("ctx-9", inner)
	convSink, ok := reg.ConversationSink("ctx-9")
	if !ok {
		t.Fatal("conversation sink not registered")
	}

	decorated := decoratedProviderSink{StreamSink: convSink}
	target, ok := reg.Exporter(session.SinkSpec{ID: "ui", Sink: decorated})
	if !ok || target.Kind != delegation.StreamTargetKindConversation || target.ID != "ctx-9" {
		t.Fatalf("Exporter over decorated provider = %+v, %v", target, ok)
	}
	// A decorator that does not forward the provider is opaque again.
	opaque := agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	})
	if _, ok := reg.Exporter(session.SinkSpec{ID: "ui", Sink: opaque}); ok {
		t.Fatal("Exporter described an opaque sink")
	}
}

// decoratedProviderSink wraps a provider sink and passes the
// description through, the recommended way for UI decorators to stay
// recognizable.
type decoratedProviderSink struct {
	agent.StreamSink
	provider delegation.StreamTargetProvider
}

func (s decoratedProviderSink) StreamTarget() (delegation.StreamTarget, bool) {
	if s.provider != nil {
		return s.provider.StreamTarget()
	}
	if p, ok := s.StreamSink.(delegation.StreamTargetProvider); ok {
		return p.StreamTarget()
	}
	return delegation.StreamTarget{}, false
}

func TestStreamExportRegistry_BusForwardsEnvelopes(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	reg := NewStreamExportRegistry(map[string]event.Bus{"events": bus})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := bus.Subscribe(ctx, event.Pattern("nrun.>"))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	sink, err := reg.Resolver(context.Background(), delegation.StreamTarget{
		Kind: delegation.StreamTargetKindBus, ID: "events",
	})
	if err != nil {
		t.Fatalf("Resolver: %v", err)
	}
	env := event.Envelope{Subject: "nrun.run-1.agent-x.node.work"}
	if err := sink.OnDelta(context.Background(), env, agent.StreamDeltaPayload{}); err != nil {
		t.Fatalf("OnDelta: %v", err)
	}
	select {
	case got := <-sub.C():
		if got.Subject != env.Subject {
			t.Fatalf("forwarded subject = %q, want %q", got.Subject, env.Subject)
		}
	case <-time.After(time.Second):
		t.Fatal("bus sink did not forward envelope to subscribers")
	}
}

// TestStreamExportFullChainRegistryToWorker verifies the complete seam
// end-to-end: a sink attached to the submit context through the
// registry's ConversationSink is recognized by the registry exporter,
// the target is persisted in AsyncRequest, and after the in-process
// escrow is gone (simulated cross-process/restart) the worker resolves
// the target back through the registry resolver and the subagent's
// stream deltas reach the registered conversation sink with lineage
// headers.
func TestStreamExportFullChainRegistryToWorker(t *testing.T) {
	received := make(chan event.Envelope, 16)
	reg := NewStreamExportRegistry(nil)
	reg.RegisterConversation("ctx-1", agent.StreamSinkFunc(func(
		_ context.Context,
		env event.Envelope,
		_ agent.StreamDeltaPayload,
	) error {
		received <- env
		return nil
	}))

	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		ctx = agent.WithRunInfo(ctx, run.Info())
		if err := agent.EmitStreamPart(ctx, host, run.RunID,
			"writer.node.work", message.TextPart{Text: "async hi"}); err != nil {
			return nil, err
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		if err := publishRunEvent(ctx, host, agent.SubjectRunEnd(run.RunID), run); err != nil {
			return nil, err
		}
		return board, nil
	})
	result := buildStreamExportDeployment(t, engine)
	directory := delegation.NewDirectory()
	if err := directory.Bind(result); err != nil {
		t.Fatal(err)
	}
	backend := newStreamExportQueueBackend()
	service, err := delegation.NewService(directory, backend,
		delegation.WithStreamTargetResolver(reg.Resolver),
		delegation.WithStreamTargetExporter(reg.Exporter),
		delegation.WithStreamEscrowTTL(20*time.Millisecond),
		delegation.WithMaxConcurrency(1),
		delegation.WithDeferredWorkers(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if err := service.BindSessionManager(newStreamExportSessionManager(t, result)); err != nil {
		t.Fatal(err)
	}

	convSink, ok := reg.ConversationSink("ctx-1")
	if !ok {
		t.Fatal("conversation sink missing after register")
	}
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:   "ui",
			Sink: convSink,
		}},
		Inheritable: true,
	})
	ctx = agent.WithRunInfo(ctx, agent.RunInfo{
		Identity: agent.Identity{AgentID: "caller-agent", RunID: "run-caller"},
	})
	ctx = tool.WithCallID(ctx, "call-delegate-1")
	accepted, err := service.Delegate(ctx, delegation.Request{
		Mode: delegation.ModeAsync, Target: "writer", Input: "do it",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if accepted.Status != delegation.StatusAccepted {
		t.Fatalf("status = %q, want accepted", accepted.Status)
	}
	submitted := <-backend.submitted
	if submitted.Stream == nil || submitted.Stream.Target == nil ||
		submitted.Stream.Target.Kind != delegation.StreamTargetKindConversation ||
		submitted.Stream.Target.ID != "ctx-1" {
		t.Fatalf("exporter seam: stream = %+v", submitted.Stream)
	}

	// Simulate a worker that lost the in-process escrow (TTL expiry,
	// restart): start workers only after the escrow entry expired so
	// the resolver path is exercised.
	time.Sleep(80 * time.Millisecond)
	if err := service.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case completed := <-backend.completed:
		if completed.Status != delegation.StatusSucceeded {
			t.Fatalf("completed = %+v", completed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not complete async work")
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-received:
			if env.ParentRunID() != "run-caller" || env.ToolCallID() != "call-delegate-1" {
				t.Fatalf("lineage headers = %+v", env.Headers)
			}
			return
		case <-deadline:
			t.Fatal("registry-resolved stream envelope never received")
		}
	}
}

// streamExportEngineFactory builds the "writer" agent used by the
// full-chain test.
type streamExportEngineFactory struct {
	engine agent.Engine
}

func (f streamExportEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "agent.Engine", Impl: "stream-export-test"}
}

func (f streamExportEngineFactory) New(context.Context, resource.Input) (any, error) {
	return f.engine, nil
}

func buildStreamExportDeployment(t *testing.T, engine agent.Engine) *deploy.Result {
	t.Helper()
	reg := resource.NewRegistry()
	if err := reg.Register(streamExportEngineFactory{engine: engine}); err != nil {
		t.Fatal(err)
	}
	doc := deploy.Document{
		Version: "v1",
		Agents: map[string]agent.Definition{
			"writer": {
				Card:   agent.AgentCard{Name: "Writer", Description: "Writes"},
				Engine: agent.EngineRef{Kind: "agent.Engine", Impl: "stream-export-test"},
			},
		},
	}
	result, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = result.Close() })
	return result
}

type streamExportResultResolver struct {
	result *deploy.Result
}

func (r streamExportResultResolver) Instance(id string) (*agent.Agent, bool) {
	return r.result.Agent(id)
}

type streamExportTestHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h streamExportTestHost) Publish(ctx context.Context, env event.Envelope) error {
	return h.bus.Publish(ctx, env)
}

func newStreamExportSessionManager(t *testing.T, result *deploy.Result) *session.Manager {
	t.Helper()
	bus := event.NewMemoryBus()
	router := event.NewRouter(bus)
	factory := session.HostFactoryFunc(func(
		_ context.Context,
		request session.HostRequest,
	) (agent.Host, error) {
		return agent.HostFuncs{
			Inner:        streamExportTestHost{bus: bus},
			InterruptsFn: func() <-chan agent.Interrupt { return request.Interrupts },
			AskUserFn:    request.AskUser,
		}, nil
	})
	manager, err := session.NewManager(
		streamExportResultResolver{result: result},
		factory,
		router,
		session.WithIdleTimeout(time.Minute),
		session.WithSinkBufferSize(8),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		_ = router.Close()
		_ = bus.Close()
	})
	return manager
}

type streamExportQueueBackend struct {
	submitted chan delegation.AsyncRequest
	work      chan delegation.Work
	completed chan delegation.Response
	next      atomic.Int64
}

func newStreamExportQueueBackend() *streamExportQueueBackend {
	return &streamExportQueueBackend{
		submitted: make(chan delegation.AsyncRequest, 4),
		work:      make(chan delegation.Work, 4),
		completed: make(chan delegation.Response, 4),
	}
}

func (b *streamExportQueueBackend) Submit(
	_ context.Context,
	req delegation.AsyncRequest,
) (string, error) {
	id := "async-" + strconv.Itoa(int(b.next.Add(1)))
	b.submitted <- req
	b.work <- delegation.Work{ID: id, LeaseToken: "lease-" + id, Request: req}
	return id, nil
}

func (b *streamExportQueueBackend) Status(
	context.Context,
	string,
) (delegation.Response, error) {
	return delegation.Response{Status: delegation.StatusRunning}, nil
}

func (b *streamExportQueueBackend) Claim(ctx context.Context) (delegation.Work, error) {
	select {
	case work := <-b.work:
		return work, nil
	case <-ctx.Done():
		return delegation.Work{}, ctx.Err()
	}
}

func (b *streamExportQueueBackend) Complete(
	_ context.Context,
	_ string,
	_ string,
	response delegation.Response,
) error {
	b.completed <- response
	return nil
}
