package runtime

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

type baseHostFactory struct {
	bus event.Bus
}

func newBaseHostFactory(bus event.Bus) (session.HostFactory, error) {
	if isNil(bus) {
		return nil, errdefs.Validationf("runtime host: event bus is required")
	}
	return &baseHostFactory{bus: bus}, nil
}

func (f *baseHostFactory) NewHost(_ context.Context, request session.HostRequest) (agent.Host, error) {
	if f == nil || isNil(f.bus) {
		return nil, errdefs.Internalf("runtime host: event bus is unavailable")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return &baseHost{
		bus:        f.bus,
		interrupts: request.Interrupts,
		askUser:    request.AskUser,
	}, nil
}

type baseHost struct {
	agent.NoopHost

	bus        event.Bus
	interrupts <-chan agent.Interrupt
	askUser    session.AskUserFunc
}

func (h *baseHost) Publish(ctx context.Context, envelope event.Envelope) error {
	return h.bus.Publish(ctx, envelope)
}

func (h *baseHost) Interrupts() <-chan agent.Interrupt { return h.interrupts }

func (h *baseHost) AskUser(ctx context.Context, prompt agent.UserPrompt) (agent.UserReply, error) {
	return h.askUser(ctx, prompt)
}

// EventBus returns the borrowed deployment bus used by Publish.
func (h *baseHost) EventBus() event.Bus { return h.bus }

var (
	_ agent.Host             = (*baseHost)(nil)
	_ agent.EventBusProvider = (*baseHost)(nil)
)
