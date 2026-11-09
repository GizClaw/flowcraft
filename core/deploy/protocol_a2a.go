package deploy

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// A2AProtocol is the built-in exposure: an agent reachable over A2A.
//
// It is a registration, not a default: a manager only exposes what a deployment
// passed to WithProtocols, so "not exposed" stays the default.
func A2AProtocol() Protocol {
	return Protocol{
		Name: "a2a",
		Decode: func(settings json.RawMessage) (any, error) {
			return a2a.DecodeSettings(settings)
		},
		Build: func(cfg ProtocolSpec, deployed *Agent) (protocol, error) {
			settings, ok := cfg.Decoded.(a2a.ServerSettings)
			if !ok {
				return nil, errdefs.Validationf(
					"manage: a2a settings were not decoded; load the document instead of building the spec by hand")
			}
			card := settings.Card
			if card.Name == "" {
				// The card is named after the agent: only the manager knows it.
				card.Name = deployed.Name()
			}
			var taskStore taskstore.Store
			if deployed.spec.Store != nil {
				adapted, err := a2a.NewTaskStore(deployed.spec.Store, deployed.digest)
				if err != nil {
					return nil, err
				}
				taskStore = adapted
			}
			server, err := a2a.NewServer(a2a.ServerConfig{
				Card:      card,
				TaskStore: taskStore,
				Sessions: func(ctx context.Context, exec *a2asrv.ExecutorContext) (a2a.Runner, error) {
					return deployed.protocolRunner(ctx, exec.ContextID)
				},
				OnUndelivered: deployed.requeue,
				OnCancel:      deployed.cancelRemote,
			})
			if err != nil {
				return nil, err
			}
			return a2aSurface{server: server}, nil
		},
	}
}

// a2aSurface is the mounted handler.
type a2aSurface struct{ server *a2a.Server }

func (s a2aSurface) Handler() http.Handler { return s.server }
func (a2aSurface) Close() error            { return nil }

// protocolRunner opens a conversation for the protocol path.
//
// It takes the same "one Run per conversation" slot as Submit, so a client
// request and a background submission cannot drive the same session at once.
func (a *Agent) protocolRunner(ctx context.Context, conversationID string) (a2a.Runner, error) {
	a.mu.Lock()
	if a.closed || a.draining {
		a.mu.Unlock()
		return nil, errdefs.NotAvailablef("manage: agent %q is not accepting work", a.spec.Name)
	}
	entry, err := a.conversationLocked(ctx, conversationID)
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if entry.running {
		a.mu.Unlock()
		return nil, errdefs.NotAvailablef(
			"manage: conversation %q already has a run in flight", conversationID)
	}
	entry.running = true
	entry.done = make(chan struct{})
	// A protocol run is a run: Drain and Close must see it, and the agent's own
	// context must be able to stop it.
	a.working.Add(1)
	a.mu.Unlock()
	return &protocolRunner{agent: a, entry: entry, conversationID: conversationID}, nil
}

// protocolRunner drives one conversation and releases the slot when the stream
// ends, however it ends.
type protocolRunner struct {
	agent          *Agent
	entry          *conversation
	conversationID string
}

func (r *protocolRunner) State() agent.TaskState { return r.entry.session.State() }

func (r *protocolRunner) SetIdentity(taskID, conversationID string) {
	r.entry.session.SetIdentity(taskID, conversationID)
}

func (r *protocolRunner) Run(_ context.Context, in agent.Input) iter.Seq2[agent.Event, error] {
	// The agent's context, not the request's: a Run outlives the HTTP request,
	// and Close must be able to cancel it.
	return r.wrap(r.entry.session.Run(r.agent.runContext(r.agent.base, r.entry), in))
}

func (r *protocolRunner) Resume(_ context.Context, reply agent.Reply) iter.Seq2[agent.Event, error] {
	return r.wrap(r.entry.session.Resume(r.agent.runContext(r.agent.base, r.entry), reply))
}

func (r *protocolRunner) wrap(sequence iter.Seq2[agent.Event, error]) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		defer r.agent.working.Done()
		defer r.agent.finish(r.entry)
		// After releasing the slot, deliver whatever was requeued: the protocol
		// path has no caller to pump it, unlike Submit's drive loop.
		defer r.agent.pump(r.entry, r.conversationID)
		for event, err := range sequence {
			if !yield(event, err) {
				return
			}
		}
	}
}

// remoteCanceller is a conversation whose work continues on another machine, so
// a cancel has to be forwarded.
type remoteCanceller interface {
	CancelRemote(ctx context.Context)
}

// cancelRemote forwards a client's cancel to the peer, when the conversation is
// proxied. A local conversation needs nothing here: its run is already
// signalled through the execution's context.
func (a *Agent) cancelRemote(conversationID string) {
	a.mu.Lock()
	entry, found := a.sessions[conversationID]
	a.mu.Unlock()
	if !found {
		return
	}
	if canceller, ok := entry.session.(remoteCanceller); ok {
		canceller.CancelRemote(a.base)
	}
}

// requeue puts undelivered input back for the next Task of the conversation.
func (a *Agent) requeue(conversationID string, messages []message.Message) {
	a.mu.Lock()
	entry, found := a.sessions[conversationID]
	a.mu.Unlock()
	if !found {
		return
	}
	for _, msg := range messages {
		if _, err := entry.session.Send(a.base, msg, agent.ModeQueue); err != nil {
			return
		}
	}
}
