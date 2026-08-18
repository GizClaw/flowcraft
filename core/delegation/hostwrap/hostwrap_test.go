package hostwrap_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/delegation/hostwrap"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

type fakeDelegationService struct{}

func (*fakeDelegationService) Delegate(context.Context, sdkdelegation.Request) (sdkdelegation.Response, error) {
	return sdkdelegation.Response{}, nil
}

func (*fakeDelegationService) Get(context.Context, string) (sdkdelegation.Response, error) {
	return sdkdelegation.Response{}, nil
}

type fakeDeployment struct {
	names  []string
	values map[string]any
}

func (d *fakeDeployment) Names() []string { return d.names }
func (d *fakeDeployment) Value(name string) (any, bool) {
	value, ok := d.values[name]
	return value, ok
}

type eventBusHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h *eventBusHost) EventBus() event.Bus { return h.bus }

func hostRequest() session.HostRequest {
	return session.HostRequest{
		Key:        session.Key{AgentID: "bot", ContextID: "conv"},
		RunID:      "run-1",
		Interrupts: make(chan agent.Interrupt),
		AskUser: func(context.Context, agent.UserPrompt) (agent.UserReply, error) {
			return agent.UserReply{}, nil
		},
	}
}

func TestWrapExposesDelegationService(t *testing.T) {
	service := &fakeDelegationService{}
	inner := session.HostFactoryFunc(func(_ context.Context, _ session.HostRequest) (agent.Host, error) {
		return agent.NoopHost{}, nil
	})
	factory, err := hostwrap.Wrap(inner, &fakeDeployment{
		names:  []string{"delegation"},
		values: map[string]any{"delegation": service},
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	host, err := factory.NewHost(context.Background(), hostRequest())
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	got, ok := sdkdelegation.ServiceFromHost(host)
	if !ok || got != service {
		t.Fatalf("ServiceFromHost = (%v, %v), want (%v, true)", got, ok, service)
	}
}

func TestWrapWithoutServiceLeavesFactoryUnchanged(t *testing.T) {
	inner := session.HostFactoryFunc(func(_ context.Context, _ session.HostRequest) (agent.Host, error) {
		return agent.NoopHost{}, nil
	})
	factory, err := hostwrap.Wrap(inner, &fakeDeployment{
		names:  []string{"events"},
		values: map[string]any{"events": event.NewMemoryBus()},
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if reflect.ValueOf(factory).Pointer() != reflect.ValueOf(inner).Pointer() {
		t.Fatalf("Wrap returned a different factory without a delegation service")
	}
}

func TestWrapRejectsMultipleDelegationServices(t *testing.T) {
	inner := session.HostFactoryFunc(func(_ context.Context, _ session.HostRequest) (agent.Host, error) {
		return agent.NoopHost{}, nil
	})
	_, err := hostwrap.Wrap(inner, &fakeDeployment{
		names: []string{"a", "b"},
		values: map[string]any{
			"a": &fakeDelegationService{},
			"b": &fakeDelegationService{},
		},
	})
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("Wrap error = %v, want conflict", err)
	}
}

func TestWrapRejectsNilDeployment(t *testing.T) {
	inner := session.HostFactoryFunc(func(_ context.Context, _ session.HostRequest) (agent.Host, error) {
		return agent.NoopHost{}, nil
	})
	if _, err := hostwrap.Wrap(inner, nil); !errdefs.IsValidation(err) {
		t.Fatalf("Wrap error = %v, want validation", err)
	}
}

func TestWrapPreservesInnerEventBus(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	service := &fakeDelegationService{}
	inner := session.HostFactoryFunc(func(_ context.Context, _ session.HostRequest) (agent.Host, error) {
		return &eventBusHost{bus: bus}, nil
	})
	factory, err := hostwrap.Wrap(inner, &fakeDeployment{
		names:  []string{"delegation"},
		values: map[string]any{"delegation": service},
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	host, err := factory.NewHost(context.Background(), hostRequest())
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	got, ok := agent.EventBusFromHost(host)
	if !ok || got != bus {
		t.Fatalf("EventBusFromHost = (%v, %v), want (%v, true)", got, ok, bus)
	}
}

func TestWrapRejectsNilInnerFactory(t *testing.T) {
	_, err := hostwrap.Wrap(nil, &fakeDeployment{})
	if err == nil {
		t.Fatalf("Wrap with nil inner factory: want error")
	}
}
