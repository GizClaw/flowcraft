package runtime

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

type baseHostFactory struct {
	bus         event.Bus
	checkpoints agent.CheckpointStore
}

func newBaseHostFactory(bus event.Bus, checkpoints ...agent.CheckpointStore) (session.HostFactory, error) {
	if ptr.IsNil(bus) {
		return nil, errdefs.Validationf("runtime host: event bus is required")
	}
	var store agent.CheckpointStore
	for _, candidate := range checkpoints {
		if !ptr.IsNil(candidate) {
			store = candidate
			break
		}
	}
	return &baseHostFactory{bus: bus, checkpoints: store}, nil
}

func (f *baseHostFactory) NewHost(_ context.Context, request session.HostRequest) (agent.Host, error) {
	if f == nil || ptr.IsNil(f.bus) {
		return nil, errdefs.Internalf("runtime host: event bus is unavailable")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return &baseHost{
		bus:         f.bus,
		checkpoints: f.checkpoints,
		interrupts:  request.Interrupts,
		askUser:     request.AskUser,
	}, nil
}

type baseHost struct {
	agent.NoopHost

	bus         event.Bus
	checkpoints agent.CheckpointStore
	interrupts  <-chan agent.Interrupt
	askUser     session.AskUserFunc
}

func (h *baseHost) Publish(ctx context.Context, envelope event.Envelope) error {
	return h.bus.Publish(ctx, envelope)
}

func (h *baseHost) Interrupts() <-chan agent.Interrupt { return h.interrupts }

func (h *baseHost) AskUser(ctx context.Context, prompt agent.UserPrompt) (agent.UserReply, error) {
	return h.askUser(ctx, prompt)
}

// Checkpoint persists cp through the configured CheckpointStore, or
// drops it when no store is configured (the NoopHost default).
func (h *baseHost) Checkpoint(ctx context.Context, cp agent.Checkpoint) error {
	if h.checkpoints == nil {
		return nil
	}
	return h.checkpoints.Save(ctx, cp)
}

// CheckpointCapabilities tells engines whether this host persists anything and
// whether the configured store takes incremental records. It is the reason an
// engine can stop building checkpoint records it would only discard: without
// it, the board snapshot that becomes the Checkpoint argument is paid for
// before the no-op is discovered.
func (h *baseHost) CheckpointCapabilities() agent.CheckpointCapabilities {
	if h.checkpoints == nil {
		return agent.CheckpointCapabilities{}
	}
	caps := agent.CheckpointCapabilities{Enabled: true}
	if _, ok := h.checkpoints.(agent.CheckpointJournal); ok {
		caps.Journal = h
	}
	return caps
}

// WriteBase is [agent.CheckpointJournal] over the configured store: a base
// record replaces whatever the store held for that execution.
func (h *baseHost) WriteBase(ctx context.Context, cp agent.Checkpoint) error {
	return h.Checkpoint(ctx, cp)
}

// Append is [agent.CheckpointJournal] over a store that takes incremental
// records. A store without that capability makes the writer fall back to a
// base, which is why this reports [agent.ErrNeedsBase] instead of an error.
func (h *baseHost) Append(ctx context.Context, delta agent.CheckpointDelta) error {
	journal, ok := h.checkpoints.(agent.CheckpointJournal)
	if !ok {
		return agent.ErrNeedsBase
	}
	return journal.Append(ctx, delta)
}

// EventBus returns the borrowed deployment bus used by Publish.
func (h *baseHost) EventBus() event.Bus { return h.bus }

var (
	_ agent.Host              = (*baseHost)(nil)
	_ agent.Checkpointer      = (*baseHost)(nil)
	_ agent.EventBusProvider  = (*baseHost)(nil)
	_ agent.CheckpointTarget  = (*baseHost)(nil)
	_ agent.CheckpointJournal = (*baseHost)(nil)
)
