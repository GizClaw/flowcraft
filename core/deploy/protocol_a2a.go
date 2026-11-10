package deploy

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/server"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// A2AProtocol is the built-in exposure: an agent reachable over A2A.
//
// It is a registration, not a default: a manager only exposes what a
// deployment passed to WithProtocols, so "not exposed" stays the default.
func A2AProtocol() Protocol {
	return Protocol{
		Name: "a2a",
		Decode: func(settings json.RawMessage) (any, error) {
			return server.DecodeSettings(settings)
		},
		Build: func(cfg ProtocolSpec, deployed *Agent) (protocol, error) {
			settings, ok := cfg.Decoded.(server.ServerSettings)
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
				adapted, err := server.NewTaskStore(deployed.spec.Store, deployed.digest)
				if err != nil {
					return nil, err
				}
				taskStore = adapted
			}
			built, err := server.NewServer(server.ServerConfig{
				Card:        card,
				TaskStore:   taskStore,
				Sessions:    deployed.sessionRunner,
				ArtifactURL: deployed.spec.ArtifactURL,
			})
			if err != nil {
				return nil, err
			}
			return a2aSurface{server: built}, nil
		},
	}
}

// a2aSurface is the mounted handler.
type a2aSurface struct{ server *server.Server }

func (s a2aSurface) Handler() http.Handler { return s.server }
func (a2aSurface) Close() error            { return nil }

// sessionRunner hands the protocol one conversation's engine.
//
// The protocol binds the task id it issued; the conversation table decides
// whether that message starts a task or answers one.
func (a *Agent) sessionRunner(_ context.Context, exec *a2asrv.ExecutorContext) (agent.Runner, error) {
	return &conversationRunner{
		conversations:  a.conversations,
		conversationID: exec.ContextID,
		taskID:         string(exec.TaskID),
	}, nil
}

// conversationRunner is one conversation as the protocol sees it: Run, Cancel
// and the state it reports. It holds no lifecycle of its own — the table owns
// the slot, and Close is the host's, not the protocol's.
type conversationRunner struct {
	conversations  *agent.Agent
	conversationID string
	taskID         string
}

func (r *conversationRunner) State() agent.TaskState {
	state, _ := r.conversations.State(context.Background(), r.conversationID)
	return state
}

func (r *conversationRunner) Run(ctx context.Context, msg message.Message) iter.Seq2[agent.Event, error] {
	return r.conversations.Run(ctx, r.conversationID, msg, agent.WithTaskID(r.taskID))
}

func (r *conversationRunner) Cancel(ctx context.Context, reason string) error {
	return r.conversations.Cancel(ctx, r.conversationID, reason)
}

func (*conversationRunner) Close() error { return nil }
