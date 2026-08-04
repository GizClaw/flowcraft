package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	eventconfig "github.com/GizClaw/flowcraft/sdkx/event/config"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

const (
	integrationEngineKind   = "runtime-integration"
	integrationResourceKind = "test.RuntimeResource"
	integrationKind         = "test.lifecycle"
)

type integrationEngineFactory struct {
	identities chan agent.Identity
	delivered  <-chan struct{}
	buses      chan event.Bus
}

func (f *integrationEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{Kind: integrationEngineKind}
}

func (f *integrationEngineFactory) New(context.Context, agent.Config) (agent.Engine, error) {
	return agent.EngineFunc(func(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
		bus, ok := agent.EventBusFromHost(host)
		if !ok || bus == nil {
			return board, errors.New("runtime host does not expose EventBus")
		}
		f.identities <- run.Identity
		f.buses <- bus

		if err := publishRunEvent(ctx, host, agent.SubjectRunStart(run.RunID), run); err != nil {
			return board, err
		}
		if err := agent.EmitStreamToken(ctx, host, run.RunID, run.AgentID+".integration", "hello"); err != nil {
			return board, err
		}
		for range 2 {
			select {
			case <-f.delivered:
			case <-ctx.Done():
				return board, ctx.Err()
			}
		}

		board.AppendChannelMessage(
			agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "hello"),
		)
		if err := publishRunEvent(ctx, host, agent.SubjectRunEnd(run.RunID), run); err != nil {
			return board, err
		}
		return board, nil
	}), nil
}

func publishRunEvent(ctx context.Context, host agent.Host, subject event.Subject, run agent.Run) error {
	envelope, err := event.NewEnvelope(ctx, subject, nil)
	if err != nil {
		return err
	}
	envelope.SetRunID(run.RunID)
	envelope.SetAgentID(run.AgentID)
	return host.Publish(ctx, envelope)
}

type lifecycleLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *lifecycleLog) add(entry string) {
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	l.mu.Unlock()
}

func (l *lifecycleLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

type integrationResourceFactory struct {
	log *lifecycleLog
}

func (f integrationResourceFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{Kind: integrationResourceKind, Impl: "tracked"}
}

func (f integrationResourceFactory) New(context.Context, deploy.ResourceInput) (any, error) {
	f.log.add("resource.new")
	return &integrationResource{log: f.log}, nil
}

type integrationResource struct {
	log *lifecycleLog
}

func (r *integrationResource) Close() error {
	r.log.add("resource.close")
	return nil
}

type lifecycleIntegrationFactory struct {
	log *lifecycleLog
}

func (f *lifecycleIntegrationFactory) Spec() runtimecore.IntegrationSpec {
	return runtimecore.IntegrationSpec{
		Kind: integrationKind,
		Deps: []runtimecore.DependencySpec{{
			Name:     "state",
			Kind:     integrationResourceKind,
			Type:     reflect.TypeFor[*integrationResource](),
			Required: true,
		}},
	}
}

func (f *lifecycleIntegrationFactory) Prepare(
	context.Context,
	runtimecore.PrepareInput,
) (runtimecore.PreparedIntegration, error) {
	f.log.add("integration.prepare")
	return &lifecycleIntegration{log: f.log}, nil
}

type lifecycleIntegration struct {
	log *lifecycleLog
}

func (i *lifecycleIntegration) Bind(_ context.Context, input runtimecore.BindInput) error {
	if _, err := runtimecore.DependencyAs[*integrationResource](input.Dependencies, "state"); err != nil {
		return err
	}
	if _, ok := input.Deployment.Instance("echo"); !ok {
		return errors.New("deployed echo agent is unavailable during Bind")
	}
	i.log.add("integration.bind")
	return nil
}

func (i *lifecycleIntegration) DecorateHost(base session.HostFactory) (session.HostFactory, error) {
	i.log.add("integration.decorate")
	return session.HostFactoryFunc(func(ctx context.Context, request session.HostRequest) (agent.Host, error) {
		i.log.add("integration.host")
		return base.NewHost(ctx, request)
	}), nil
}

func (i *lifecycleIntegration) Start(context.Context) error {
	i.log.add("integration.start")
	return nil
}

func (i *lifecycleIntegration) Close() error {
	i.log.add("integration.close")
	return nil
}

func TestRuntimePublicAPIEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	delivered := make(chan struct{}, 2)
	identities := make(chan agent.Identity, 1)
	buses := make(chan event.Bus, 1)
	engines := agent.NewRegistry()
	engines.MustRegister(&integrationEngineFactory{
		identities: identities,
		delivered:  delivered,
		buses:      buses,
	})

	log := &lifecycleLog{}
	deployment := deploy.NewBuilder(engines)
	deployment.MustRegisterResource(eventconfig.NewMemoryDeployFactory())
	deployment.MustRegisterResource(integrationResourceFactory{log: log})

	doc, err := deploy.Parse([]byte(`
version: v1
resources:
  events:
    kind: event.Bus
    impl: memory
  runtime_state:
    kind: test.RuntimeResource
    impl: tracked
agents:
  echo:
    engine:
      kind: runtime-integration
runtime:
  event_bus: events
  sessions:
    idle_timeout: 1m
    sink_buffer: 8
  integrations:
    - name: lifecycle
      kind: test.lifecycle
      deps:
        state: runtime_state
`))
	if err != nil {
		t.Fatal(err)
	}

	builder := runtimecore.NewBuilder(deployment)
	if err := builder.RegisterIntegration(&lifecycleIntegrationFactory{log: log}); err != nil {
		t.Fatal(err)
	}
	app, err := builder.Build(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}

	lease, err := app.Sessions().Open(ctx, session.Key{
		AgentID:   "echo",
		ContextID: "session-context",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	received := [2]chan agent.StreamDeltaPayload{
		make(chan agent.StreamDeltaPayload, 1),
		make(chan agent.StreamDeltaPayload, 1),
	}
	sinks := make([]session.SinkSpec, len(received))
	for index := range received {
		index := index
		sinks[index] = session.SinkSpec{
			ID: fmt.Sprintf("sink-%d", index),
			Sink: agent.StreamSinkFunc(func(
				_ context.Context,
				envelope event.Envelope,
				delta agent.StreamDeltaPayload,
			) error {
				if agent.IsStreamDelta(envelope.Subject) {
					received[index] <- delta
					delivered <- struct{}{}
				}
				return nil
			}),
		}
	}

	turn, err := lease.Session().Start(ctx, agent.Request{
		ContextID: "caller-context-must-be-replaced",
		RunID:     "caller-run-must-be-replaced",
		Message:   message.NewTextMessage(message.RoleUser, "hello"),
	}, sinks...)
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCompleted || result.RunID != turn.RunID() {
		t.Fatalf("result = %+v, turn RunID = %q", result, turn.RunID())
	}
	if turn.RunID() == "caller-run-must-be-replaced" {
		t.Fatal("Session did not replace the caller-supplied RunID")
	}

	identity := <-identities
	if identity.AgentID != "echo" || identity.ConversationID != "session-context" {
		t.Fatalf("engine identity = %+v", identity)
	}
	if identity.RunID != turn.RunID() {
		t.Fatalf("engine RunID = %q, turn RunID = %q", identity.RunID, turn.RunID())
	}
	if bus := <-buses; bus == nil {
		t.Fatal("engine received a nil EventBus from the runtime host")
	}
	for index, events := range received {
		select {
		case delta := <-events:
			if delta.Type != agent.StreamDeltaToken || delta.Content != "hello" {
				t.Fatalf("sink %d delta = %+v", index, delta)
			}
		case <-ctx.Done():
			t.Fatalf("sink %d did not receive the stream event: %v", index, ctx.Err())
		}
	}

	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Sessions().Open(context.Background(), session.Key{
		AgentID: "echo", ContextID: "after-close",
	}); !errors.Is(err, session.ErrManagerClosed) {
		t.Fatalf("Open after Runtime.Close error = %v, want ErrManagerClosed", err)
	}

	entries := log.snapshot()
	wantBuildOrder := []string{
		"integration.prepare",
		"resource.new",
		"integration.bind",
		"integration.decorate",
		"integration.start",
		"integration.host",
	}
	if len(entries) < len(wantBuildOrder) || !slices.Equal(entries[:len(wantBuildOrder)], wantBuildOrder) {
		t.Fatalf("build lifecycle = %v, want prefix %v", entries, wantBuildOrder)
	}
	closeIntegration := slices.Index(entries, "integration.close")
	closeResource := slices.Index(entries, "resource.close")
	if closeIntegration < 0 || closeResource < 0 || closeIntegration > closeResource {
		t.Fatalf("close lifecycle = %v; integration must close before deployment resource", entries)
	}
}
