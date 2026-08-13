package delegation_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/event"
)

type serviceStub struct{}

func (*serviceStub) Delegate(context.Context, delegation.Request) (delegation.Response, error) {
	return delegation.Response{}, nil
}

func (*serviceStub) Get(context.Context, string) (delegation.Response, error) {
	return delegation.Response{}, nil
}

type eventBusHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h *eventBusHost) EventBus() event.Bus { return h.bus }

func TestServiceFromHost(t *testing.T) {
	service := &serviceStub{}

	t.Run("direct wrapper", func(t *testing.T) {
		host := delegation.WithService(agent.NoopHost{}, service)
		got, ok := delegation.ServiceFromHost(host)
		if !ok || got != service {
			t.Fatalf("ServiceFromHost = (%v, %v), want (%v, true)", got, ok, service)
		}
	})

	t.Run("agent decorators preserve capability", func(t *testing.T) {
		host := agent.ComposeHost(
			delegation.WithService(agent.NoopHost{}, service),
			agent.TracingMiddleware(),
		)
		got, ok := delegation.ServiceFromHost(host)
		if !ok || got != service {
			t.Fatalf("ServiceFromHost(decorated) = (%v, %v), want (%v, true)", got, ok, service)
		}
	})

	t.Run("nil host", func(t *testing.T) {
		if got, ok := delegation.ServiceFromHost(nil); ok || got != nil {
			t.Fatalf("ServiceFromHost(nil) = (%v, %v)", got, ok)
		}
	})

	t.Run("typed nil service", func(t *testing.T) {
		var service *serviceStub
		host := delegation.WithService(agent.NoopHost{}, service)
		if got, ok := delegation.ServiceFromHost(host); ok || got != nil {
			t.Fatalf("ServiceFromHost(typed nil) = (%v, %v)", got, ok)
		}
	})

	t.Run("custom publisher blocks inner capability", func(t *testing.T) {
		host := agent.HostFuncs{
			Inner: delegation.WithService(agent.NoopHost{}, service),
			PublishFn: func(context.Context, event.Envelope) error {
				return nil
			},
		}
		if got, ok := delegation.ServiceFromHost(host); ok || got != nil {
			t.Fatalf("ServiceFromHost(custom publisher) = (%v, %v), want (nil, false)", got, ok)
		}
	})
}

func TestWithServicePreservesInnerEventBus(t *testing.T) {
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	service := &serviceStub{}

	t.Run("service wrapper preserves event bus", func(t *testing.T) {
		host := delegation.WithService(&eventBusHost{bus: bus}, service)
		got, ok := agent.EventBusFromHost(host)
		if !ok || got != bus {
			t.Fatalf("EventBusFromHost(WithService) = (%v, %v), want (%v, true)", got, ok, bus)
		}
	})

	t.Run("custom publisher remains authoritative", func(t *testing.T) {
		host := agent.HostFuncs{
			Inner: delegation.WithService(&eventBusHost{bus: bus}, service),
			PublishFn: func(context.Context, event.Envelope) error {
				return nil
			},
		}
		if got, ok := agent.EventBusFromHost(host); ok || got != nil {
			t.Fatalf("EventBusFromHost(custom publisher) = (%v, %v), want (nil, false)", got, ok)
		}
	})
}
