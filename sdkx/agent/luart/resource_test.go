package luart

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestDeployFactorySpec(t *testing.T) {
	want := deploy.ResourceSpec{Kind: ResourceKind, Impl: "lua"}
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
max_exec_time: 50ms
`,
			configured: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := NewDeployFactory().New(context.Background(), deploy.ResourceInput{
				Settings: luaSettingsNode(t, tt.settings),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			rt, ok := value.(*Runtime)
			if !ok {
				t.Fatalf("New returned %T, want *luart.Runtime", value)
			}
			defer func() { _ = rt.Close() }()
			var _ agent.ScriptRuntime = rt
			if tt.configured {
				if rt.poolSize != 1 || rt.maxExecTime != 50*time.Millisecond {
					t.Fatalf("runtime settings = pool %d, duration %s",
						rt.poolSize, rt.maxExecTime)
				}
				if rt.SupportsNestedExec() {
					t.Fatal("configured single-state runtime supports nested execution")
				}
			}
			if _, err := rt.Exec(context.Background(), "smoke", `local x = 1`, nil); err != nil {
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
		"max_exec_time: nonsense\n",
		"max_exec_time: -1s\n",
	}
	for _, settings := range tests {
		t.Run(settings, func(t *testing.T) {
			if _, err := NewDeployFactory().New(context.Background(), deploy.ResourceInput{
				Settings: luaSettingsNode(t, settings),
			}); err == nil {
				t.Fatal("New accepted invalid settings")
			}
		})
	}
}

func TestDeployFactoryAcceptsZeroExecTime(t *testing.T) {
	value, err := NewDeployFactory().New(context.Background(), deploy.ResourceInput{
		Settings: luaSettingsNode(t, "max_exec_time: 0s\n"),
	})
	if err != nil {
		t.Fatalf("New rejected zero max_exec_time: %v", err)
	}
	_ = value.(*Runtime).Close()
}

func TestLuaRuntimeLifecycleOwnedByDeployResult(t *testing.T) {
	registry := agent.NewRegistry()
	registry.MustRegister(luaResourceEngineFactory{})
	builder := deploy.NewBuilder(registry)
	builder.MustRegisterResource(NewDeployFactory())
	doc, err := deploy.Parse([]byte(`
version: v1
resources:
  scripts:
    kind: agent.ScriptRuntime
    impl: lua
    settings: {pool_size: 1}
agents:
  test:
    engine: {kind: lua-resource-test}
    deps: {runtime: scripts}
`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := deploy.ResourceAs[agent.ScriptRuntime](result, "scripts")
	if err != nil {
		t.Fatalf("ResourceAs[agent.ScriptRuntime]: %v", err)
	}
	luaRuntime, ok := rt.(*Runtime)
	if !ok {
		t.Fatalf("resource = %T, want *luart.Runtime", rt)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("first Result.Close: %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("second Result.Close: %v", err)
	}
	if _, err := luaRuntime.Exec(context.Background(), "closed", `return nil`, nil); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("Exec after Result.Close error = %v, want ErrRuntimeClosed", err)
	}
}

func TestRuntimeCloseWithCheckedOutVM(t *testing.T) {
	runtime := New(WithPoolSize(1))
	state, err := runtime.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	waiter := make(chan error, 1)
	go func() {
		_, err := runtime.acquire(context.Background())
		waiter <- err
	}()

	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	if err := <-waiter; !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("blocked acquire after Close = %v, want ErrRuntimeClosed", err)
	}

	runtime.release(state)
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := runtime.acquire(context.Background()); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("acquire after Close = %v, want ErrRuntimeClosed", err)
	}
}

func TestRuntimeCloseCancelsActiveExec(t *testing.T) {
	runtime := New(WithPoolSize(1))
	runtime.init()
	execDone := make(chan error, 1)
	go func() {
		_, err := runtime.Exec(context.Background(), "loop", `while true do end`, nil)
		execDone <- err
	}()

	deadline := time.After(2 * time.Second)
	for len(runtime.pool) != 0 {
		select {
		case <-deadline:
			t.Fatal("Exec did not acquire the VM")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-execDone:
		if err == nil {
			t.Fatal("active Exec returned nil after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active Exec did not stop before Close returned")
	}
}

type luaResourceEngineFactory struct{}

func (luaResourceEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{
		Kind: "lua-resource-test",
		Deps: []agent.DepSpec{{Name: "runtime", Type: ResourceKind, Required: true}},
	}
}

func (luaResourceEngineFactory) New(context.Context, agent.Config) (agent.Engine, error) {
	return agent.EngineFunc(func(
		context.Context,
		agent.Run,
		agent.Host,
		*agent.Board,
	) (*agent.Board, error) {
		board := agent.NewBoard()
		board.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return board, nil
	}), nil
}

func luaSettingsNode(t *testing.T, input string) *yamlv3.Node {
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
