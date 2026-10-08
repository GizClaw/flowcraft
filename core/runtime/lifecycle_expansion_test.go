package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
)

// captureEngineFactory records the settings of the most recent engine
// construction, so tests can assert how dynamic registrations expand.
type captureEngineFactory struct {
	mu       sync.Mutex
	settings []byte
}

func (f *captureEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: testEngineKind, Impl: testEngineImpl}
}

func (f *captureEngineFactory) New(
	_ context.Context,
	in resource.Input,
) (any, error) {
	f.mu.Lock()
	f.settings = append([]byte(nil), in.Settings...)
	f.mu.Unlock()
	return simpleRunEngine(), nil
}

func (f *captureEngineFactory) captured() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.settings)
}

// TestDynamicAgentExpandsLikeDeployment verifies the parity a host
// relies on when agents arrive after the build: a dynamically
// registered agent expands custom schemes and ${secret:...} exactly
// like a deployment-time one, both on RegisterAgent and on the Reload
// re-bind path.
func TestDynamicAgentExpandsLikeDeployment(t *testing.T) {
	factory := &captureEngineFactory{}
	reg := resource.NewRegistry()
	reg.MustRegister(factory)
	reg.MustRegister(freshBusFactory{})
	if err := secret.Register(reg); err != nil {
		t.Fatalf("secret.Register: %v", err)
	}
	doc := parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: test}
  secrets: {kind: secret.Store, impl: env, settings: {id: env, default: true}}
agents:
  bot:
    card: {name: Bot}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
`)
	builder := NewBuilder(reg)
	if err := builder.WithResolver(resource.NewResolver(
		resource.SchemeFunc{
			SchemeName: "test",
			Fn: func(context.Context, resource.Reference) (any, error) {
				return "custom-value", nil
			},
		},
	)); err != nil {
		t.Fatalf("WithResolver: %v", err)
	}
	app, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	definition := agent.Definition{
		Card: agent.AgentCard{Name: "Dyn"},
		Engine: agent.EngineRef{
			Kind: testEngineKind,
			Impl: testEngineImpl,
			Settings: []byte(`{
				"scheme": "${test:anything}",
				"secret": "${secret:DYNAMIC_KEY}"
			}`),
		},
	}
	if _, err := app.RegisterAgent(
		context.Background(), "dyn", definition); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	assertExpanded := func(stage string) {
		t.Helper()
		settings := factory.captured()
		if !strings.Contains(settings, "custom-value") {
			t.Fatalf("%s: settings %s did not expand the custom scheme", stage, settings)
		}
		if !strings.Contains(settings, `"store":"env"`) ||
			!strings.Contains(settings, `"name":"DYNAMIC_KEY"`) {
			t.Fatalf("%s: settings %s did not expand ${secret:...}", stage, settings)
		}
	}
	assertExpanded("RegisterAgent")

	if _, err := app.Reload(context.Background(), doc); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	assertExpanded("Reload re-bind")
}
