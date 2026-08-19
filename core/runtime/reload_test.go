package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

const reloadEngineKind = "reload-engine"

// gatedEngineFactory builds engines that report their generation and
// block on a shared gate until it is closed, so tests can hold a turn
// in flight across a reload.
type gatedEngineFactory struct {
	gen  atomic.Int64
	runs chan int64
	gate chan struct{}
}

func (f *gatedEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: reloadEngineKind}
}

func (f *gatedEngineFactory) New(context.Context, resource.Input) (any, error) {
	gen := f.gen.Add(1)
	return agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		select {
		case f.runs <- gen:
		default:
		}
		_ = publishRunEvent(ctx, host, agent.SubjectRunStart(run.RunID), run)
		select {
		case <-f.gate:
		case <-ctx.Done():
			return board, ctx.Err()
		}
		board.AppendChannelMessage(
			agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "ok"))
		_ = publishRunEvent(ctx, host, agent.SubjectRunEnd(run.RunID), run)
		return board, nil
	}), nil
}

func waitForGen(t *testing.T, runs <-chan int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case got := <-runs:
			if got == want {
				return
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("engine generation %d not observed", want)
}

// waitForEngineRun blocks until the gated engine reports one run and
// returns the observed generation. Multi-agent builds instantiate engines
// in resource order, which is not deterministic, so tests must not couple
// a wait to a specific generation number — any run from the turn under test
// is enough to know it reached the engine and is blocked on the gate.
func waitForEngineRun(t *testing.T, runs <-chan int64) int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case got := <-runs:
			return got
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("engine run not observed")
	return 0
}

// recordedBusFactory records every bus it builds so tests can publish
// on the exact old/new generation buses.
type recordedBusFactory struct {
	built chan event.Bus
}

func (f recordedBusFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "event.Bus", Impl: "recorded"}
}

func (f recordedBusFactory) New(context.Context, resource.Input) (any, error) {
	bus := event.NewMemoryBus()
	f.built <- bus
	return bus, nil
}

// sharedCheckpointBacking is one storage layer shared by every store
// handle a factory produces, simulating a store that survives a
// rebuild (e.g. the same sqlite file behind two handles).
type sharedCheckpointBacking struct {
	mu  sync.Mutex
	cps map[string]agent.Checkpoint
}

type sharedCheckpointStore struct{ backing *sharedCheckpointBacking }

func (s *sharedCheckpointStore) Save(_ context.Context, cp agent.Checkpoint) error {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	s.backing.cps[cp.ExecID] = cp.Clone()
	return nil
}

func (s *sharedCheckpointStore) Load(_ context.Context, id string) (*agent.Checkpoint, error) {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	cp, ok := s.backing.cps[id]
	if !ok {
		return nil, nil
	}
	clone := cp.Clone()
	return &clone, nil
}

func (s *sharedCheckpointStore) Delete(_ context.Context, id string) error {
	s.backing.mu.Lock()
	defer s.backing.mu.Unlock()
	delete(s.backing.cps, id)
	return nil
}

type sharedStoreFactory struct{ backing *sharedCheckpointBacking }

func (f sharedStoreFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "agent.CheckpointStore", Impl: "shared"}
}

func (f sharedStoreFactory) New(context.Context, resource.Input) (any, error) {
	return &sharedCheckpointStore{backing: f.backing}, nil
}

// noDeleteStore implements the CheckpointStore contract without
// CheckpointDeleter, for resume-contract validation tests.
type noDeleteStore struct{}

func (noDeleteStore) Save(context.Context, agent.Checkpoint) error { return nil }
func (noDeleteStore) Load(context.Context, string) (*agent.Checkpoint, error) {
	return nil, nil
}

type noDeleteStoreFactory struct{}

func (noDeleteStoreFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "agent.CheckpointStore", Impl: "nodelete"}
}

func (noDeleteStoreFactory) New(context.Context, resource.Input) (any, error) {
	return noDeleteStore{}, nil
}

// appendEngineFactory completes every turn by appending one assistant
// message, so tests can count conversation history across reloads.
type appendEngineFactory struct{}

func (appendEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "append-engine"}
}

func (appendEngineFactory) New(context.Context, resource.Input) (any, error) {
	return agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		_ = publishRunEvent(ctx, host, agent.SubjectRunStart(run.RunID), run)
		board.AppendChannelMessage(
			agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "ok"))
		_ = publishRunEvent(ctx, host, agent.SubjectRunEnd(run.RunID), run)
		return board, nil
	}), nil
}

// recordSink collects envelopes for runtime-level Attach assertions.
type recordSink struct {
	mu  sync.Mutex
	got []event.Envelope
}

func (s *recordSink) OnEnvelope(_ context.Context, env event.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, env)
	return nil
}

func (s *recordSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func reloadDoc(t *testing.T, agents, extra string) deploy.Document {
	t.Helper()
	return parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: memory}
`+extra+`
agents:
`+agents+`
runtime:
  event_bus: events
  sessions: {idle_timeout: 1m, sink_buffer: 8}
`)
}

func TestRuntimeReloadSwapsGenerationWithInFlightTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	f := &gatedEngineFactory{
		runs: make(chan int64, 8),
		gate: make(chan struct{}),
	}
	reg := resource.NewRegistry()
	reg.MustRegister(f)
	reg.MustRegister(event.NewFactory())

	doc1 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: reload-engine}
`, "")
	app, err := NewBuilder(reg).Build(ctx, doc1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	lease, err := app.Sessions().Open(ctx, session.Key{
		AgentID: "bot", ContextID: "ctx",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	turn, err := lease.Session().Start(ctx, agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForGen(t, f.runs, 1)

	// Reload while the turn is blocked on the gate.
	doc2 := reloadDoc(t, `  bot:
    card: {name: Bot v2}
    engine: {kind: reload-engine}
`, "")
	result, err := app.Reload(ctx, doc2)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if result.GenerationID != 2 || result.PreviousID != 1 {
		t.Fatalf("ReloadResult = %+v, want generation 2 over 1", result)
	}
	if _, ok := app.Agent("bot"); !ok {
		t.Fatal("bot not resolvable after reload")
	}

	// The in-flight turn completes on generation 1.
	close(f.gate)
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("in-flight turn Wait: %v", err)
	}

	// The next turn runs on generation 2.
	next, err := lease.Session().Start(ctx, agent.Request{})
	if err != nil {
		t.Fatalf("Start after reload: %v", err)
	}
	waitForGen(t, f.runs, 2)
	if _, err := next.Wait(ctx); err != nil {
		t.Fatalf("next turn Wait: %v", err)
	}
}

const markerEngineKind = "marker-engine"

type generationMarker interface {
	GenerationMarker() int
}

// markerHost tags a host with the generation whose result-aware decorator
// produced it, without replacing any underlying capability.
type markerHost struct {
	agent.Host
	gen int
}

func (h markerHost) GenerationMarker() int  { return h.gen }
func (h markerHost) UnwrapHost() agent.Host { return h.Host }

type markerEngineFactory struct {
	markers chan int
}

func (f markerEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: markerEngineKind}
}

func (f markerEngineFactory) New(context.Context, resource.Input) (any, error) {
	return agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		marker := 0
		if m, ok := host.(generationMarker); ok {
			marker = m.GenerationMarker()
		}
		select {
		case f.markers <- marker:
		default:
		}
		board.AppendChannelMessage(
			agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "ok"))
		return board, nil
	}), nil
}

func TestRuntimeReloadReappliesResultHostDecorator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	engine := &markerEngineFactory{markers: make(chan int, 8)}
	reg := resource.NewRegistry()
	reg.MustRegister(engine)
	reg.MustRegister(event.NewFactory())

	builder := NewBuilder(reg)
	var calls atomic.Int64
	var mu sync.Mutex
	var results []*deploy.Result
	if err := builder.WithResultHostFactory(func(
		result *deploy.Result,
		factory session.HostFactory,
	) (session.HostFactory, error) {
		gen := int(calls.Add(1))
		mu.Lock()
		results = append(results, result)
		mu.Unlock()
		return session.HostFactoryFunc(func(
			ctx context.Context,
			request session.HostRequest,
		) (agent.Host, error) {
			host, err := factory.NewHost(ctx, request)
			if err != nil {
				return nil, err
			}
			return markerHost{Host: host, gen: gen}, nil
		}), nil
	}); err != nil {
		t.Fatalf("WithResultHostFactory: %v", err)
	}

	doc1 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: marker-engine}
`, "")
	app, err := builder.Build(ctx, doc1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	if marker := runMarkerTurn(t, ctx, app, engine.markers, 1); marker != 1 {
		t.Fatalf("generation 1 turn marker = %d, want 1", marker)
	}

	doc2 := reloadDoc(t, `  bot:
    card: {name: Bot v2}
    engine: {kind: marker-engine}
`, "")
	if _, err := app.Reload(ctx, doc2); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if marker := runMarkerTurn(t, ctx, app, engine.markers, 2); marker != 2 {
		t.Fatalf("generation 2 turn marker = %d, want 2", marker)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != 2 {
		t.Fatalf("decorator calls = %d, want 2 (one per generation)", len(results))
	}
	if results[0] == results[1] {
		t.Fatalf("decorator received the same result for both generations")
	}
}

func runMarkerTurn(
	t *testing.T,
	ctx context.Context,
	app *Runtime,
	markers <-chan int,
	wantCall int,
) int {
	t.Helper()
	lease, err := app.Sessions().Open(ctx, session.Key{
		AgentID: "bot", ContextID: "ctx",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	turn, err := lease.Session().Start(ctx, agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("turn %d Wait: %v", wantCall, err)
	}
	select {
	case marker := <-markers:
		return marker
	case <-ctx.Done():
		t.Fatalf("marker %d not observed: %v", wantCall, ctx.Err())
		return 0
	}
}

func TestRuntimeReloadAllowsBusConfigChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	built := make(chan event.Bus, 4)
	reg := resource.NewRegistry()
	reg.MustRegister(recordedBusFactory{built: built})
	reg.MustRegister(appendEngineFactory{})

	doc1 := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: recorded, settings: {route_cache_size: 1024}}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1m, sink_buffer: 8}
`)
	app, err := NewBuilder(reg).Build(ctx, doc1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	bus1 := <-built

	sink := &recordSink{}
	detach, err := app.Attach(ctx, event.Pattern("reload.bus.*"), sink)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(detach)

	// The new document changes the bus resource settings: reload must
	// accept it, attach the new bus to the router, and retire bus1.
	doc2 := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: recorded, settings: {route_cache_size: 2048}}
agents:
  bot:
    card: {name: Bot v2}
    engine: {kind: append-engine}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1m, sink_buffer: 8}
`)
	if _, err := app.Reload(ctx, doc2); err != nil {
		t.Fatalf("Reload with bus config change: %v", err)
	}
	bus2 := <-built
	if app.bus != bus2 {
		t.Fatal("runtime bus not switched to the new generation's bus")
	}

	publishOn(t, bus2, "reload.bus.after")
	waitForCount(t, sink, 1)
	// The old bus was removed and closed with the retired generation:
	// publishing on it must not reach the attachment.
	_ = bus1.Publish(context.Background(), event.Envelope{
		Subject: event.Subject("reload.bus.old"),
	})
	time.Sleep(50 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("old bus delivery after reload = %d, want 1", sink.count())
	}
}

func TestRuntimeReloadAllowsCheckpointConfigChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	backing := &sharedCheckpointBacking{cps: make(map[string]agent.Checkpoint)}
	reg := resource.NewRegistry()
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(sharedStoreFactory{backing: backing})
	reg.MustRegister(appendEngineFactory{})

	doc1 := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: memory}
  cps: {kind: agent.CheckpointStore, impl: shared, settings: {window: 10}}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
runtime:
  event_bus: events
  checkpoint_store: cps
  sessions: {idle_timeout: 1m, sink_buffer: 8, resume: true}
`)
	app, err := NewBuilder(reg).Build(ctx, doc1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	first := runTurn(t, app.Sessions(), "bot", "ctx")
	if got := assistantCount(first.LastBoard); got != 1 {
		t.Fatalf("first turn messages = %d, want 1", got)
	}

	// Reload with different checkpoint store settings (same backing).
	doc2 := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: memory}
  cps: {kind: agent.CheckpointStore, impl: shared, settings: {window: 40}}
agents:
  bot:
    card: {name: Bot v2}
    engine: {kind: append-engine}
runtime:
  event_bus: events
  checkpoint_store: cps
  sessions: {idle_timeout: 1m, sink_buffer: 8, resume: true}
`)
	if _, err := app.Reload(ctx, doc2); err != nil {
		t.Fatalf("Reload with checkpoint config change: %v", err)
	}

	// Same session key continues on the new store handle; the committed
	// history from the first turn must carry over.
	second := runTurn(t, app.Sessions(), "bot", "ctx")
	if got := assistantCount(second.LastBoard); got != 2 {
		t.Fatalf("second turn messages = %d, want 2 (history preserved)", got)
	}
}

func TestRuntimeReloadRebindsDynamicAgents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reg := resource.NewRegistry()
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(appendEngineFactory{})
	doc := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
`, "")
	app, err := NewBuilder(reg).Build(ctx, doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	instance, err := app.RegisterAgent(ctx, "dyn", agent.Definition{
		Card: agent.AgentCard{Name: "Dyn"},
		Engine: agent.EngineRef{
			Kind: "append-engine",
		},
	})
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if instance == nil {
		t.Fatal("RegisterAgent returned nil instance")
	}

	if _, err := app.Reload(ctx, doc); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := app.Agent("dyn"); !ok {
		t.Fatal("dynamic agent lost after reload")
	}
	runTurn(t, app.Sessions(), "dyn", "ctx")
}

func TestRuntimeReloadDrainsRemovedAgents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reg := resource.NewRegistry()
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(appendEngineFactory{})
	doc1 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
  extra:
    card: {name: Extra}
    engine: {kind: append-engine}
`, "")
	app, err := NewBuilder(reg).Build(ctx, doc1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	runTurn(t, app.Sessions(), "extra", "ctx")
	doc2 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
`, "")
	result, err := app.Reload(ctx, doc2)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(result.DrainedAgents) != 1 || result.DrainedAgents[0] != "extra" {
		t.Fatalf("DrainedAgents = %v, want [extra]", result.DrainedAgents)
	}
	if _, err := app.Sessions().Open(ctx, session.Key{
		AgentID: "extra", ContextID: "ctx",
	}); !errdefs.IsNotFound(err) {
		t.Fatalf("Open(removed agent) error = %v, want not found", err)
	}
	runTurn(t, app.Sessions(), "bot", "ctx")
}

func TestRuntimeReloadDrainTimeoutClearsTombstone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	f := &gatedEngineFactory{
		runs: make(chan int64, 8),
		gate: make(chan struct{}),
	}
	reg := resource.NewRegistry()
	reg.MustRegister(f)
	reg.MustRegister(event.NewFactory())
	doc1 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: reload-engine}
  extra:
    card: {name: Extra}
    engine: {kind: reload-engine}
`, "")
	app, err := NewBuilder(reg).Build(ctx, doc1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	// Keep "extra" busy so the reload drain cannot finish within the
	// short reload context.
	lease, err := app.Sessions().Open(ctx, session.Key{
		AgentID: "extra", ContextID: "ctx",
	})
	if err != nil {
		t.Fatalf("Open(extra): %v", err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	turn, err := lease.Session().Start(ctx, agent.Request{})
	if err != nil {
		t.Fatalf("Start(extra): %v", err)
	}
	waitForEngineRun(t, f.runs)

	reloadCtx, cancelReload := context.WithTimeout(
		context.Background(), 100*time.Millisecond)
	defer cancelReload()
	doc2 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: reload-engine}
`, "")
	if _, err := app.Reload(reloadCtx, doc2); !errdefs.IsTimeout(err) {
		t.Fatalf("Reload error = %v, want deadline exceeded", err)
	}
	// The failed reload must leave the old generation fully serving:
	// the tombstone "extra" tried to drain is cleared.
	if _, err := app.Sessions().Open(context.Background(), session.Key{
		AgentID: "extra", ContextID: "ctx",
	}); err != nil {
		t.Fatalf("Open(extra) after failed reload = %v, want nil", err)
	}
	close(f.gate)
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("in-flight turn Wait: %v", err)
	}
}

func TestRuntimeReloadFailureKeepsOldGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reg := resource.NewRegistry()
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(appendEngineFactory{})
	doc1 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
`, "")
	app, err := NewBuilder(reg).Build(ctx, doc1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	doc2 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
`, `  bad: {kind: nope.Thing, impl: x}`)
	if _, err := app.Reload(ctx, doc2); err == nil {
		t.Fatal("Reload with unresolvable resource succeeded, want error")
	}
	runTurn(t, app.Sessions(), "bot", "ctx")
	if app.current.id != 1 {
		t.Fatalf("generation id after failed reload = %d, want 1", app.current.id)
	}
}

func TestRuntimeReloadResumeRequiresDeleterStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reg := resource.NewRegistry()
	reg.MustRegister(event.NewFactory())
	reg.MustRegister(noDeleteStoreFactory{})
	reg.MustRegister(appendEngineFactory{})
	doc1 := reloadDoc(t, `  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
`, "")
	app, err := NewBuilder(reg).Build(ctx, doc1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	doc2 := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: memory}
  cps: {kind: agent.CheckpointStore, impl: nodelete}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
runtime:
  event_bus: events
  checkpoint_store: cps
  sessions: {idle_timeout: 1m, sink_buffer: 8, resume: true}
`)
	if _, err := app.Reload(ctx, doc2); !errdefs.IsValidation(err) {
		t.Fatalf("Reload resume-without-deleter error = %v, want validation", err)
	}
}

// TestRuntimeReloadRejectsSharedBusFactory guards the ownership
// assumption that every generation owns its own bus: a factory that
// returns the current generation's bus must be rejected without the
// aborted result closing the shared bus.
func TestRuntimeReloadRejectsSharedBusFactory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	reg := resource.NewRegistry()
	reg.MustRegister(singletonBusFactory{bus: bus})
	reg.MustRegister(appendEngineFactory{})
	doc := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: singleton}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: append-engine}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1m, sink_buffer: 8}
`)
	app, err := NewBuilder(reg).Build(ctx, doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	if _, err := app.Reload(ctx, doc); !errdefs.IsConflict(err) {
		t.Fatalf("Reload with shared bus error = %v, want conflict", err)
	}
	// The old generation keeps serving and its bus is still open.
	runTurn(t, app.Sessions(), "bot", "ctx")
	publishOn(t, bus, "reload.shared.alive")
}

type singletonBusFactory struct{ bus event.Bus }

func (f singletonBusFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "event.Bus", Impl: "singleton"}
}

func (f singletonBusFactory) New(context.Context, resource.Input) (any, error) {
	return f.bus, nil
}

func publishOn(t *testing.T, bus event.Bus, subject string) {
	t.Helper()
	if err := bus.Publish(context.Background(), event.Envelope{
		Subject: event.Subject(subject),
	}); err != nil {
		t.Fatalf("Publish(%s): %v", subject, err)
	}
}

func waitForCount(t *testing.T, sink *recordSink, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sink.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sink count = %d, want %d", sink.count(), want)
}

func assistantCount(board *agent.Board) int {
	if board == nil {
		return 0
	}
	count := 0
	for _, m := range board.Channel(agent.MainChannel) {
		if m.Role == message.RoleAssistant {
			count++
		}
	}
	return count
}
