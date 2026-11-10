package deploy

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/server"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// A2AProtocol is the built-in exposure: an agent reachable over A2A.
//
// It is a registration, not a default: a manager only exposes what a
// deployment's document names. Handler options are host wiring, so they are
// captured here instead of living on the spec.
func A2AProtocol(options ...a2asrv.RequestHandlerOption) ProtocolKind {
	return ProtocolKind{
		Name: "a2a",
		Decode: func(settings json.RawMessage) (any, error) {
			return server.DecodeSettings(settings)
		},
		Build: func(_ context.Context, deps ProtocolDeps) (Protocol, error) {
			settings, err := server.DecodeSettings(deps.Spec.Settings)
			if err != nil {
				return nil, err
			}
			card := settings.Card
			if card.Name == "" {
				// The card is named after the agent: only the manager knows it.
				card.Name = deps.Name
			}
			var taskStore taskstore.Store
			if deps.Store != nil {
				adapted, err := server.NewTaskStore(deps.Store, deps.Digest)
				if err != nil {
					return nil, err
				}
				taskStore = adapted
			}
			built, err := server.NewServer(server.ServerConfig{
				Card:        card,
				TaskStore:   taskStore,
				ArtifactURL: deps.ArtifactURL,
				Runners: func(ctx context.Context, exec *a2asrv.ExecutorContext) (agent.Runner, error) {
					return deps.Runner(ctx, exec.ContextID, string(exec.TaskID))
				},
				Options: options,
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
