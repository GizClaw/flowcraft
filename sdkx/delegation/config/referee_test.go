package config_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/tool"
	delegationconfig "github.com/GizClaw/flowcraft/sdkx/delegation/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestHandoffRefereeFactorySettings(t *testing.T) {
	factory := delegationconfig.NewHandoffRefereeFactory(&mutableDirectory{})
	for name, settings := range map[string]*yamlv3.Node{
		"omitted": nil,
		"empty":   settingsNode(t, "{}"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := factory(context.Background(), deploy.HookInput{Settings: settings}); err != nil {
				t.Fatalf("factory: %v", err)
			}
		})
	}
	if _, err := factory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, "target: billing"),
	}); !errdefs.IsValidation(err) {
		t.Fatalf("unknown setting error = %v, want Validation", err)
	}
}

func TestHandoffRefereeFactoryCapturesUnboundDirectory(t *testing.T) {
	directory := &mutableDirectory{}
	referee, err := delegationconfig.NewHandoffRefereeFactory(directory)(
		context.Background(),
		deploy.HookInput{},
	)
	if err != nil {
		t.Fatal(err)
	}
	directory.target = sdkdelegation.Target{
		ID:    "billing",
		Modes: []sdkdelegation.Mode{sdkdelegation.ModeHandoff},
	}
	result := &agent.Result{Messages: []inference.Message{
		toolCall("first", "billing"),
		successfulToolResult("first"),
		toolCall("second", "billing"),
		successfulToolResult("second"),
	}}
	decision, err := referee.After(context.Background(), agent.Identity{}, &agent.Request{}, result)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Reason != sdkdelegation.HandoffFinalizeReason+"billing" {
		t.Fatalf("decision = %+v", decision)
	}
	event, ok := sdkdelegation.HandoffFromResult(result)
	if !ok || event.ToolCallID != "first" {
		t.Fatalf("event = %+v, found = %v", event, ok)
	}
}

func TestHandoffRefereeCanBeEnabledFromAgentYAML(t *testing.T) {
	registry := agent.NewRegistry()
	if err := registry.Register(fakeEngineFactory{}); err != nil {
		t.Fatal(err)
	}
	builder := deploy.NewBuilder(registry)
	builder.RegisterReferee(
		delegationconfig.RefereeType,
		delegationconfig.NewHandoffRefereeFactory(&mutableDirectory{}),
	)
	document, err := deploy.Parse([]byte(`
version: v1
agents:
  coordinator:
    engine:
      kind: fake
    referees:
      - type: delegation_handoff
`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(context.Background(), document)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = result.Close() })
	if _, ok := result.Instance("coordinator"); !ok {
		t.Fatal("coordinator instance missing")
	}
}

type mutableDirectory struct {
	target sdkdelegation.Target
}

func (*mutableDirectory) List(context.Context) ([]sdkdelegation.Target, error) {
	return nil, errdefs.NotAvailablef("not bound")
}

func (d *mutableDirectory) Get(_ context.Context, id string) (sdkdelegation.Target, error) {
	if d.target.ID != id {
		return sdkdelegation.Target{}, sdkdelegation.TargetNotFound(id)
	}
	return d.target, nil
}

type fakeEngineFactory struct{}

func (fakeEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{Kind: "fake"}
}

func (fakeEngineFactory) New(context.Context, agent.Config) (agent.Engine, error) {
	return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		return board, nil
	}), nil
}

func toolCall(id, target string) inference.Message {
	arguments, _ := json.Marshal(map[string]any{
		"mode":   sdkdelegation.ModeHandoff,
		"target": target,
		"input":  "refund",
	})
	return inference.Message{
		Role: inference.RoleAssistant,
		Content: inference.Content{Parts: []inference.Part{
			inference.ToolCallPart{Call: tool.Call{
				ID:        id,
				Name:      sdkdelegation.ToolName,
				Arguments: arguments,
			}},
		}},
	}
}

func successfulToolResult(callID string) inference.Message {
	return inference.Message{
		Role: inference.RoleTool,
		Content: inference.Content{Parts: []inference.Part{
			inference.ToolResultPart{Result: tool.Result{CallID: callID}},
		}},
	}
}

func settingsNode(t *testing.T, input string) *yamlv3.Node {
	t.Helper()
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(input), &node); err != nil {
		t.Fatal(err)
	}
	return node.Content[0]
}
