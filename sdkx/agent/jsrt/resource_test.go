package jsrt

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestDeployFactorySpec(t *testing.T) {
	want := deploy.ResourceSpec{Kind: ResourceKind, Impl: "js"}
	if got := NewDeployFactory().Spec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryBuildsDefaultAndConfiguredRuntime(t *testing.T) {
	tests := []struct {
		name       string
		settings   string
		configured bool
	}{
		{name: "default"},
		{
			name: "configured",
			settings: `
pool_size: 1
max_call_stack_size: 64
max_exec_time: 50ms
`,
			configured: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := NewDeployFactory().New(context.Background(), deploy.ResourceInput{
				Settings: jsSettingsNode(t, tt.settings),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			rt, ok := value.(*Runtime)
			if !ok {
				t.Fatalf("New returned %T, want *jsrt.Runtime", value)
			}
			var _ agent.ScriptRuntime = rt
			if tt.configured {
				if rt.poolSize != 1 || rt.maxCallStackSize != 64 ||
					rt.maxExecTime != 50*time.Millisecond {
					t.Fatalf(
						"runtime settings = pool %d, stack %d, duration %s",
						rt.poolSize, rt.maxCallStackSize, rt.maxExecTime)
				}
				if rt.SupportsNestedExec() {
					t.Fatal("configured single-VM runtime supports nested execution")
				}
			}
			if _, err := rt.Exec(context.Background(), "smoke", `var x = 1`, nil); err != nil {
				t.Fatalf("Exec: %v", err)
			}
		})
	}
}

func TestDeployFactoryRejectsInvalidSettings(t *testing.T) {
	tests := []string{
		"unknown: true\n",
		"pool_size: 0\n",
		"pool_size: -1\n",
		"max_call_stack_size: 0\n",
		"max_call_stack_size: -1\n",
		"max_exec_time: nonsense\n",
		"max_exec_time: -1s\n",
	}
	for _, settings := range tests {
		t.Run(settings, func(t *testing.T) {
			if _, err := NewDeployFactory().New(context.Background(), deploy.ResourceInput{
				Settings: jsSettingsNode(t, settings),
			}); err == nil {
				t.Fatal("New accepted invalid settings")
			}
		})
	}
}

func TestDeployFactoryAcceptsZeroExecTime(t *testing.T) {
	if _, err := NewDeployFactory().New(context.Background(), deploy.ResourceInput{
		Settings: jsSettingsNode(t, "max_exec_time: 0s\n"),
	}); err != nil {
		t.Fatalf("New rejected zero max_exec_time: %v", err)
	}
}

func TestScriptRuntimeBuildAndResourceAs(t *testing.T) {
	registry := agent.NewRegistry()
	registry.MustRegister(jsResourceEngineFactory{})
	builder := deploy.NewBuilder(registry)
	builder.MustRegisterResource(NewDeployFactory())
	doc, err := deploy.Parse([]byte(`
version: v1
resources:
  scripts:
    kind: agent.ScriptRuntime
    impl: js
    settings: {pool_size: 1}
agents:
  test:
    engine: {kind: js-resource-test}
    deps: {runtime: scripts}
`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = result.Close() }()
	rt, err := deploy.ResourceAs[agent.ScriptRuntime](result, "scripts")
	if err != nil {
		t.Fatalf("ResourceAs[agent.ScriptRuntime]: %v", err)
	}
	if _, ok := rt.(*Runtime); !ok {
		t.Fatalf("resource = %T, want *jsrt.Runtime", rt)
	}
}

type jsResourceEngineFactory struct{}

func (jsResourceEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{
		Kind: "js-resource-test",
		Deps: []agent.DepSpec{{Name: "runtime", Type: ResourceKind, Required: true}},
	}
}

func (jsResourceEngineFactory) New(context.Context, agent.Config) (agent.Engine, error) {
	return agent.EngineFunc(func(
		context.Context,
		agent.Run,
		agent.Host,
		*agent.Board,
	) (*agent.Board, error) {
		board := agent.NewBoard()
		board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "ok"))
		return board, nil
	}), nil
}

func jsSettingsNode(t *testing.T, input string) *yamlv3.Node {
	t.Helper()
	if input == "" {
		return nil
	}
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(input), &node); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return node.Content[0]
}
