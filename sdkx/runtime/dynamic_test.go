package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	"github.com/GizClaw/flowcraft/sdkx/tool/dynamic"
)

// exposureSource is a config source that registers tools and publishes
// their exposure metadata onto a session catalog, mirroring how the MCP
// source carries config-declared exposure into the dynamic layer.
type exposureSource struct {
	names    []string
	exposure dynamic.Exposure
}

func (s *exposureSource) Attach(_ context.Context, reg *sdktool.Registry) error {
	for _, name := range s.names {
		reg.Register(sdktool.FuncTool(message.Definition{
			Name:        name,
			Description: name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(_ context.Context, _ string) (string, error) {
			return "ran:" + name, nil
		}))
	}
	return nil
}

func (s *exposureSource) Close() error { return nil }

func (s *exposureSource) ApplyExposures(cat *dynamic.Catalog) error {
	for _, name := range s.names {
		if err := cat.SetExposure(name, s.exposure); err != nil {
			return err
		}
	}
	return nil
}

func buildExposureAssembly(t *testing.T, source *exposureSource) *toolconfig.Assembly {
	t.Helper()
	builder := toolconfig.NewBuilder(toolconfig.Deps{})
	builder.RegisterSourceFactory("exposure",
		func(context.Context, sdkconfig.Input) (toolconfig.Source, error) {
			return source, nil
		})
	assembly, err := builder.Build(context.Background(), toolconfig.Document{
		Version: toolconfig.VersionV1,
		Sources: []toolconfig.SourceEntry{{
			Kind: "exposure",
			Spec: json.RawMessage(`{}`),
		}},
	})
	if err != nil {
		t.Fatalf("tool assembly build: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })
	return assembly
}

func dynamicCatalogDoc(t *testing.T) deploy.Document {
	t.Helper()
	doc, err := deploy.Parse([]byte(`version: v1
resources:
  events: {kind: event.Bus, impl: test}
  research_tools: {kind: tool.Assembly, impl: yaml}
  assistant_tools: {kind: tool.Assembly, impl: test}
agents:
  researcher: {engine: {kind: test}}
  assistant: {engine: {kind: test}}
runtime:
  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: research_tools, assistant: assistant_tools}
      default_exposure: deferred
      exposures: {tool_search: always}
      selected_retention: 3
      recent_window: 7
      budget: {max_definitions: 20, max_bytes: 4096}
`))
	if err != nil {
		t.Fatalf("deploy.Parse: %v", err)
	}
	return doc
}

func TestBuild_DynamicCatalogMapsToolsPerAgent(t *testing.T) {
	deployBuilder := deploy.NewBuilder()
	deployBuilder.MustRegisterEngine(testEngineFactory{engine: agent.EngineFunc(
		func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
			return board, nil
		},
	)})
	if err := deployBuilder.RegisterResource(testResourceFactory{
		spec:  sdkconfig.Spec{Kind: eventBusResourceKind, Impl: "test"},
		value: event.NewMemoryBus(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := deployBuilder.RegisterResource(testResourceFactory{
		spec: sdkconfig.Spec{Kind: toolconfig.ResourceKind, Impl: "yaml"},
		value: buildExposureAssembly(t, &exposureSource{
			names:    []string{"research_tool"},
			exposure: dynamic.ExposureAlways,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := deployBuilder.RegisterResource(testResourceFactory{
		spec: sdkconfig.Spec{Kind: toolconfig.ResourceKind, Impl: "test"},
		value: buildExposureAssembly(t, &exposureSource{
			names:    []string{"assist_tool"},
			exposure: dynamic.ExposureAlways,
		}),
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	saw := map[string]sdktool.Catalog{}
	builder := NewBuilder(deployBuilder)
	if err := builder.WithHostFactory(func(base session.HostFactory) (session.HostFactory, error) {
		return session.HostFactoryFunc(func(ctx context.Context, req session.HostRequest) (agent.Host, error) {
			host, err := base.NewHost(ctx, req)
			if err != nil {
				return nil, err
			}
			if catalog, ok := sdktool.CatalogFromContext(ctx); ok {
				mu.Lock()
				saw[req.Key.AgentID] = catalog
				mu.Unlock()
			}
			return host, nil
		}), nil
	}); err != nil {
		t.Fatal(err)
	}

	runtime, err := builder.Build(context.Background(), dynamicCatalogDoc(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	runTurn := func(agentID, contextID string) {
		t.Helper()
		lease, err := runtime.Sessions().Open(context.Background(), session.Key{
			AgentID: agentID, ContextID: contextID,
		})
		if err != nil {
			t.Fatalf("Open(%s): %v", agentID, err)
		}
		defer func() { _ = lease.Close() }()
		turn, err := lease.Session().Start(context.Background(), agent.Request{})
		if err != nil {
			t.Fatalf("Start(%s): %v", agentID, err)
		}
		if _, err := turn.Wait(context.Background()); err != nil {
			t.Fatalf("Wait(%s): %v", agentID, err)
		}
	}

	runTurn("researcher", "conv")
	runTurn("assistant", "conv")
	// A second conversation of the same agent gets its own catalog.
	runTurn("researcher", "other-conv")

	mu.Lock()
	defer mu.Unlock()
	researchCat, ok := saw["researcher"].(*dynamic.Catalog)
	if !ok {
		t.Fatalf("researcher catalog type = %T, want *dynamic.Catalog", saw["researcher"])
	}
	assistCat, ok := saw["assistant"].(*dynamic.Catalog)
	if !ok {
		t.Fatalf("assistant catalog type = %T, want *dynamic.Catalog", saw["assistant"])
	}
	if researchCat == assistCat {
		t.Fatal("agents share one catalog instance")
	}

	assertDefinitions := func(cat *dynamic.Catalog, want, notWant []string) {
		t.Helper()
		names := make(map[string]bool)
		for _, def := range cat.Definitions() {
			names[def.Name] = true
		}
		for _, name := range want {
			if !names[name] {
				t.Errorf("catalog definitions missing %q: %v", name, cat.Definitions())
			}
		}
		for _, name := range notWant {
			if names[name] {
				t.Errorf("catalog unexpectedly contains %q", name)
			}
		}
	}
	assertDefinitions(researchCat,
		[]string{"tool_search", "research_tool"}, []string{"assist_tool"})
	assertDefinitions(assistCat,
		[]string{"tool_search", "assist_tool"}, []string{"research_tool"})
}

func newDynamicDeployBuilder(t *testing.T) *deploy.Builder {
	t.Helper()
	deployBuilder := deploy.NewBuilder()
	deployBuilder.MustRegisterEngine(testEngineFactory{engine: agent.EngineFunc(
		func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
			return board, nil
		},
	)})
	if err := deployBuilder.RegisterResource(testResourceFactory{
		spec:  sdkconfig.Spec{Kind: eventBusResourceKind, Impl: "test"},
		value: event.NewMemoryBus(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := deployBuilder.RegisterResource(testResourceFactory{
		spec: sdkconfig.Spec{Kind: toolconfig.ResourceKind, Impl: "yaml"},
		value: buildExposureAssembly(t, &exposureSource{
			names:    []string{"research_tool"},
			exposure: dynamic.ExposureAlways,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	return deployBuilder
}

func TestBuild_DynamicCatalogRejectsBadMappings(t *testing.T) {
	for name, tc := range map[string]struct {
		runtimeYAML string
		notFound    bool
	}{
		"missing resource": {`  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: nope}
`, true},
		"unknown agent": {`  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {ghost: research_tools}
`, false},
		"uncovered agent": {`  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: research_tools}
`, false},
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := deploy.Parse([]byte(`version: v1
resources:
  events: {kind: event.Bus, impl: test}
  research_tools: {kind: tool.Assembly, impl: yaml}
agents:
  researcher: {engine: {kind: test}}
  assistant: {engine: {kind: test}}
runtime:
` + tc.runtimeYAML))
			if err != nil {
				t.Fatalf("deploy.Parse: %v", err)
			}
			_, err = NewBuilder(newDynamicDeployBuilder(t)).Build(
				context.Background(), doc,
			)
			if err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}
			if tc.notFound && !errdefs.IsNotFound(err) {
				t.Fatalf("Build error = %v, want NotFound", err)
			}
			if !tc.notFound && !errdefs.IsValidation(err) {
				t.Fatalf("Build error = %v, want validation", err)
			}
		})
	}
}

func TestBuild_DynamicCatalogDefaultCoversAgents(t *testing.T) {
	deployBuilder := newDynamicDeployBuilder(t)
	doc, err := deploy.Parse([]byte(`version: v1
resources:
  events: {kind: event.Bus, impl: test}
  research_tools: {kind: tool.Assembly, impl: yaml}
agents:
  researcher: {engine: {kind: test}}
  assistant: {engine: {kind: test}}
runtime:
  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {default: research_tools}
`))
	if err != nil {
		t.Fatalf("deploy.Parse: %v", err)
	}

	runtime, err := NewBuilder(deployBuilder).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	lease, err := runtime.Sessions().Open(context.Background(), session.Key{
		AgentID: "assistant", ContextID: "conv",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = lease.Close() }()
	turn, err := lease.Session().Start(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
