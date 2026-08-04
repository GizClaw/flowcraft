package delegation

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
	sdkxdelegation "github.com/GizClaw/flowcraft/sdkx/delegation"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	tooldelegation "github.com/GizClaw/flowcraft/sdkx/tool/delegation"
)

type namedTool struct{ name string }

func (t namedTool) Definition() message.Definition {
	return message.DefineSchema(t.name, "existing").Build()
}

func (namedTool) Execute(context.Context, string) (string, error) { return "", nil }

func integrationSettings(t *testing.T, yaml string) *deploy.Opaque {
	t.Helper()
	doc, err := deploy.Parse([]byte(`
version: v1
runtime:
  event_bus: events
  integrations:
    - name: delegation
      kind: delegation.local
      settings:
` + yaml + `
`))
	if err != nil {
		t.Fatal(err)
	}
	config, err := runtimecore.DecodeConfig(doc)
	if err != nil {
		t.Fatal(err)
	}
	return config.Integrations[0].Settings
}

func TestFactoryRejectsNilRegistry(t *testing.T) {
	var registry *tool.Registry
	if _, err := NewFactory(registry); !errdefs.IsValidation(err) {
		t.Fatalf("NewFactory(nil) error = %v, want validation", err)
	}
}

func TestFactorySpec(t *testing.T) {
	factory, err := NewFactory(tool.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	spec := factory.Spec()
	if spec.Kind != Kind || len(spec.Deps) != 1 {
		t.Fatalf("Spec = %+v", spec)
	}
	dep := spec.Deps[0]
	if dep.Name != "backend" || dep.Kind != "delegation.AsyncBackend" || dep.Required ||
		dep.Type != reflect.TypeFor[sdkxdelegation.AsyncBackend]() {
		t.Fatalf("backend dependency = %+v", dep)
	}
}

func TestPrepareRegistersToolsBeforeDeployAndCloseRollsBack(t *testing.T) {
	registry := tool.NewRegistry()
	factory, err := NewFactory(registry)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := factory.Prepare(context.Background(), runtimecore.PrepareInput{
		Name: "delegation",
		Kind: Kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{sdkdelegation.ToolName, tooldelegation.StatusToolName} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("tool %q is not visible before deploy build", name)
		}
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if registry.Len() != 0 {
		t.Fatalf("registry length after rollback = %d, want 0", registry.Len())
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPrepareConflictDoesNotOverwriteExistingTool(t *testing.T) {
	registry := tool.NewRegistry()
	existing := namedTool{name: sdkdelegation.ToolName}
	registry.Register(existing)
	factory, err := NewFactory(registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Prepare(context.Background(), runtimecore.PrepareInput{
		Name: "delegation",
		Kind: Kind,
	}); !errdefs.IsConflict(err) {
		t.Fatalf("Prepare conflict error = %v", err)
	}
	got, ok := registry.Get(sdkdelegation.ToolName)
	if !ok || got != existing {
		t.Fatalf("existing tool was overwritten: %#v", got)
	}
}

func TestCloseDoesNotUnregisterReplacementTool(t *testing.T) {
	registry := tool.NewRegistry()
	factory, err := NewFactory(registry)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := factory.Prepare(context.Background(), runtimecore.PrepareInput{
		Name: "delegation",
		Kind: Kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement := namedTool{name: sdkdelegation.ToolName}
	registry.Register(replacement)
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	got, ok := registry.Get(sdkdelegation.ToolName)
	if !ok || got != replacement {
		t.Fatalf("replacement tool was unregistered: %#v", got)
	}
	if _, ok := registry.Get(tooldelegation.StatusToolName); ok {
		t.Fatal("integration-owned status tool was not unregistered")
	}
}

func TestPrepareSettingsAreStrict(t *testing.T) {
	registry := tool.NewRegistry()
	factory, err := NewFactory(registry)
	if err != nil {
		t.Fatal(err)
	}
	_, err = factory.Prepare(context.Background(), runtimecore.PrepareInput{
		Name:     "delegation",
		Kind:     Kind,
		Settings: integrationSettings(t, "        max_concurrency: 2\n        max_depth: 3\n        timeout: 1s\n        unknown: true"),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("unknown setting error = %v, want validation", err)
	}
	for _, invalid := range []string{
		"        max_concurrency: 0",
		"        max_depth: -1",
		"        timeout: -1s",
		"        timeout: eventually",
	} {
		if _, err := factory.Prepare(context.Background(), runtimecore.PrepareInput{
			Name: "delegation", Kind: Kind, Settings: integrationSettings(t, invalid),
		}); !errdefs.IsValidation(err) {
			t.Fatalf("invalid settings %q error = %v, want validation", invalid, err)
		}
	}
}

type deploymentView struct{}

func (deploymentView) Instance(string) (*deploy.Instance, bool) { return nil, false }
func (deploymentView) InstanceNames() []string                  { return nil }

type eventHost struct {
	agent.NoopHost
	bus event.Bus
}

func (h *eventHost) EventBus() event.Bus { return h.bus }

func TestBindDecoratesHostsAndBindsOnlyOnce(t *testing.T) {
	registry := tool.NewRegistry()
	factory, err := NewFactory(registry)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := factory.Prepare(context.Background(), runtimecore.PrepareInput{
		Name:     "delegation",
		Kind:     Kind,
		Settings: integrationSettings(t, "        max_concurrency: 1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()

	bus := event.NewMemoryBus()
	defer bus.Close()
	var workerRequest session.HostRequest
	base := session.HostFactoryFunc(func(_ context.Context, request session.HostRequest) (agent.Host, error) {
		workerRequest = request
		return &eventHost{bus: bus}, nil
	})
	input := runtimecore.BindInput{
		Name:       "delegation",
		Deployment: deploymentView{},
		BaseHosts:  base,
	}
	if err := prepared.Bind(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := workerRequest.Validate(); err != nil {
		t.Fatalf("synthetic worker HostRequest: %v", err)
	}
	if _, err := workerRequest.AskUser(context.Background(), agent.UserPrompt{}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("worker AskUser error = %v, want not available", err)
	}
	if err := prepared.Bind(context.Background(), input); !errdefs.IsConflict(err) {
		t.Fatalf("second Bind error = %v, want conflict", err)
	}

	decorated, err := prepared.DecorateHost(base)
	if err != nil {
		t.Fatal(err)
	}
	host, err := decorated.NewHost(context.Background(), session.HostRequest{
		Key:        session.Key{AgentID: "agent", ContextID: "context"},
		RunID:      "run",
		Interrupts: make(chan agent.Interrupt),
		AskUser: func(context.Context, agent.UserPrompt) (agent.UserReply, error) {
			return agent.UserReply{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sdkdelegation.ServiceFromHost(host); !ok {
		t.Fatal("decorated host does not expose delegation service")
	}
	if got, ok := agent.EventBusFromHost(host); !ok || got != bus {
		t.Fatal("decorated host lost EventBus capability")
	}
	if err := prepared.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
}
