package delegation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/delegation"
	delegationconfig "github.com/GizClaw/flowcraft/sdkx/delegation/config"
	kanbanconfig "github.com/GizClaw/flowcraft/sdkx/delegation/kanban/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	eventconfig "github.com/GizClaw/flowcraft/sdkx/event/config"
	tooldelegation "github.com/GizClaw/flowcraft/sdkx/tool/delegation"
)

type integrationEngineFactory struct{}

func (integrationEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{Kind: "integration"}
}

func (integrationEngineFactory) New(_ context.Context, config agent.Config) (agent.Engine, error) {
	behavior, _ := config.Setting("behavior")
	switch behavior {
	case "complete":
		return agent.EngineFunc(func(ctx context.Context, _ agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
			if err := host.Publish(ctx, event.Envelope{}); err != nil {
				return nil, err
			}
			board.AppendChannelMessage(agent.MainChannel,
				message.NewTextMessage(message.RoleAssistant, "completed locally"))
			return board, nil
		}), nil
	case "handoff":
		return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
			board.AppendChannelMessage(agent.MainChannel, message.Message{
				Role: message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{
					message.ToolCallPart{Call: message.Call{
						ID:        "handoff-call",
						Name:      sdkdelegation.ToolName,
						Arguments: []byte(`{"mode":"handoff","target":"worker","input":"take over"}`),
					}},
					message.ToolResultPart{Result: message.Result{
						CallID:  "handoff-call",
						Content: "accepted",
					}},
				}},
			})
			return board, nil
		}), nil
	default:
		return nil, fmt.Errorf("unknown integration behavior %q", behavior)
	}
}

type integrationHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h integrationHost) EventBus() event.Bus { return h.bus }

func (h integrationHost) Publish(ctx context.Context, envelope event.Envelope) error {
	return h.bus.Publish(ctx, envelope)
}

type toolResponse struct {
	DelegationID string               `json:"delegation_id"`
	Status       sdkdelegation.Status `json:"status"`
	Output       string               `json:"output"`
}

func TestDeployDelegationEndToEnd(t *testing.T) {
	ctx := context.Background()
	configDir := t.TempDir()
	agentFile := filepath.Join(configDir, "worker.agent.yaml")
	if err := os.WriteFile(agentFile, []byte(`
version: v1
card:
  name: Worker
  description: Completes delegated work
engine:
  kind: integration
  settings:
    behavior: complete
`), 0o600); err != nil {
		t.Fatalf("write versioned agent file: %v", err)
	}

	document, err := deploy.Parse([]byte(`
version: v1
resources:
  events:
    kind: event.Bus
    impl: memory
    export: true
  delegations:
    kind: delegation.AsyncBackend
    impl: kanban-memory
    export: true
    deps:
      event_bus: events
    settings:
      scope_id: integration
agents:
  dispatcher:
    card:
      name: Dispatcher
    engine:
      kind: integration
      settings:
        behavior: handoff
    referees:
      - type: delegation_handoff
  worker:
    file: worker.agent.yaml
`))
	if err != nil {
		t.Fatalf("parse deployment: %v", err)
	}

	directory := delegation.NewDirectory()
	delegationTools := tooldelegation.New(directory)
	engines := agent.NewRegistry()
	engines.MustRegister(integrationEngineFactory{})
	builder := deploy.NewBuilder(engines, deploy.WithBaseDir(configDir))
	builder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())
	builder.MustRegisterResource(kanbanconfig.NewMemoryDeployFactory())
	builder.RegisterReferee(
		delegationconfig.RefereeType,
		delegationconfig.NewHandoffRefereeFactory(directory),
	)

	result, err := builder.Build(ctx, document)
	if err != nil {
		t.Fatalf("build deployment: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })
	if err := directory.Bind(result); err != nil {
		t.Fatalf("bind directory: %v", err)
	}
	backend, err := deploy.ResourceAs[delegation.AsyncBackend](result, "delegations")
	if err != nil {
		t.Fatalf("get async backend: %v", err)
	}
	bus, err := deploy.ResourceAs[event.Bus](result, "events")
	if err != nil {
		t.Fatalf("get event bus: %v", err)
	}
	service, err := delegation.NewService(directory, backend, delegation.WithMaxConcurrency(2))
	if err != nil {
		t.Fatalf("create delegation service: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	host := sdkdelegation.WithService(integrationHost{bus: bus}, service)
	if got, ok := sdkdelegation.ServiceFromHost(host); !ok || got != service {
		t.Fatal("WithService host does not expose the local service")
	}
	execCtx := agent.ContextWithHost(ctx, host)

	t.Run("sync", func(t *testing.T) {
		raw, err := delegationTools[0].Execute(execCtx,
			`{"mode":"sync","target":"worker","input":"finish this"}`)
		if err != nil {
			t.Fatalf("sync delegate: %v", err)
		}
		var response toolResponse
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			t.Fatalf("decode sync response: %v", err)
		}
		if response.DelegationID == "" || response.Status != sdkdelegation.StatusSucceeded ||
			response.Output != "completed locally" {
			t.Fatalf("sync response = %+v", response)
		}
	})

	t.Run("async", func(t *testing.T) {
		raw, err := delegationTools[0].Execute(execCtx,
			`{"mode":"async","target":"worker","input":"finish later"}`)
		if err != nil {
			t.Fatalf("async delegate: %v", err)
		}
		var response toolResponse
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			t.Fatalf("decode async response: %v", err)
		}
		if response.DelegationID == "" || response.Status != sdkdelegation.StatusAccepted {
			t.Fatalf("async response = %+v", response)
		}
		delegationID := response.DelegationID
		deadline := time.Now().Add(2 * time.Second)
		for {
			raw, err = delegationTools[1].Execute(execCtx,
				fmt.Sprintf(`{"delegation_id":%q}`, delegationID))
			if err != nil {
				t.Fatalf("get async status: %v", err)
			}
			if err := json.Unmarshal([]byte(raw), &response); err != nil {
				t.Fatalf("decode async status: %v", err)
			}
			if response.Status.Terminal() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("delegation %q did not finish: %+v", delegationID, response)
			}
			time.Sleep(time.Millisecond)
		}
		if response.DelegationID != delegationID ||
			response.Status != sdkdelegation.StatusSucceeded ||
			response.Output != "completed locally" {
			t.Fatalf("terminal async response = %+v", response)
		}
	})

	t.Run("handoff referee", func(t *testing.T) {
		dispatcher, ok := result.Instance("dispatcher")
		if !ok {
			t.Fatal("dispatcher instance missing")
		}
		response, err := dispatcher.Execute(execCtx, agent.Request{
			Message: message.NewTextMessage(message.RoleUser, "route me"),
		}, agent.WithHost(host))
		if err != nil {
			t.Fatalf("execute dispatcher: %v", err)
		}
		handoff, ok := sdkdelegation.HandoffFromResult(response)
		if !ok {
			t.Fatal("structured handoff event missing")
		}
		if handoff.Target != "worker" || handoff.ToolCallID != "handoff-call" ||
			handoff.Args.Input != "take over" {
			t.Fatalf("handoff event = %+v", handoff)
		}
	})

	if err := service.Close(); err != nil {
		t.Fatalf("close service: %v", err)
	}
	if err := bus.Publish(ctx, event.Envelope{}); err != nil {
		t.Fatalf("service closed the shared event bus: %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("close deploy result: %v", err)
	}
	if err := bus.Publish(ctx, event.Envelope{}); !errors.Is(err, event.ErrBusClosed) {
		t.Fatalf("publish after result close = %v, want ErrBusClosed", err)
	}
}
