package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
)

func buildTestAssembly(t *testing.T, names ...string) *tool.Assembly {
	t.Helper()
	tools := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, funcTool(name, "ran:"+name))
	}
	assembly, err := tool.NewAssembly([]tool.Source{testSource{tools: tools}})
	if err != nil {
		t.Fatalf("tool.NewAssembly: %v", err)
	}
	t.Cleanup(func() { _ = assembly.Close() })
	return assembly
}

func dynamicCatalogDoc(t *testing.T, toolsYAML string) deploy.Document {
	t.Helper()
	return parseRuntimeDoc(t, `version: v1
resources:
  events: {kind: event.Bus, impl: test}
  research_tools: {kind: tool.Assembly, impl: yaml}
  assistant_tools: {kind: tool.Assembly, impl: test}
agents:
  researcher:
    card: {name: Researcher}
    engine: {kind: agent.Engine, impl: test}
  assistant:
    card: {name: Assistant}
    engine: {kind: agent.Engine, impl: test}
runtime:
  event_bus: events
  sessions: {idle_timeout: 1s, sink_buffer: 8}
  dynamic_catalog:
    tools:
`+toolsYAML)
}

func catalogEngine(t *testing.T, got chan<- []string) agent.Engine {
	t.Helper()
	return withRunEnd(agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		catalog, ok := tool.SessionFromContext(ctx)
		if !ok {
			return board, errors.New("tool session missing from execute context")
		}
		defs := catalog.Definitions()
		names := make([]string, 0, len(defs))
		for _, def := range defs {
			names = append(names, def.Name)
		}
		got <- names
		return board, nil
	}))
}

func TestBuild_DynamicCatalogMapsToolsPerAgent(t *testing.T) {
	got := make(chan []string, 2)
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: catalogEngine(t, got)})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: event.NewMemoryBus(),
	})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: tool.AssemblyKind, Impl: "yaml"},
		value: buildTestAssembly(t, "research_tool"),
	})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: tool.AssemblyKind, Impl: "test"},
		value: buildTestAssembly(t, "assist_tool"),
	})

	doc := dynamicCatalogDoc(t, `      researcher: research_tools
      assistant: assistant_tools
`)
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if result := runTurn(t, app.Sessions(), "researcher", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("researcher status = %q", result.Status)
	}
	if result := runTurn(t, app.Sessions(), "assistant", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("assistant status = %q", result.Status)
	}

	research := <-got
	assist := <-got
	assertDefinitions(t, research, []string{"research_tool"}, []string{"assist_tool"})
	assertDefinitions(t, assist, []string{"assist_tool"}, []string{"research_tool"})
}

func TestBuild_DynamicCatalogDefaultCoversAgents(t *testing.T) {
	got := make(chan []string, 1)
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: catalogEngine(t, got)})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: event.NewMemoryBus(),
	})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: tool.AssemblyKind, Impl: "yaml"},
		value: buildTestAssembly(t, "research_tool"),
	})

	doc := dynamicCatalogDoc(t, `      default: research_tools
`)
	delete(doc.Resources, "assistant_tools")
	app, err := NewBuilder(reg).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close() }()

	if result := runTurn(t, app.Sessions(), "assistant", "conv"); result.Status != agent.StatusCompleted {
		t.Fatalf("assistant status = %q", result.Status)
	}
	assertDefinitions(t, <-got, []string{"research_tool"}, nil)
}

func TestBuild_DynamicCatalogRejectsBadMappings(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: noopEngine()})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: event.NewMemoryBus(),
	})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: tool.AssemblyKind, Impl: "yaml"},
		value: buildTestAssembly(t, "research_tool"),
	})

	for name, toolsYAML := range map[string]string{
		"missing resource": `      researcher: nope
`,
		"unknown agent": `      ghost: research_tools
`,
		"uncovered agent": `      researcher: research_tools
`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := dynamicCatalogDoc(t, toolsYAML)
			delete(doc.Resources, "assistant_tools")
			if _, err := NewBuilder(reg).Build(context.Background(), doc); err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}
		})
	}
}

func TestBuild_DynamicCatalogRejectsTypedNilAssembly(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(testEngineFactory{engine: noopEngine()})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: testEventKind, Impl: testEventImpl},
		value: event.NewMemoryBus(),
	})
	reg.MustRegister(testResourceFactory{
		spec:  resource.Spec{Kind: tool.AssemblyKind, Impl: "yaml"},
		value: (*tool.Assembly)(nil),
	})

	doc := dynamicCatalogDoc(t, `      researcher: research_tools
      assistant: research_tools
`)
	if _, err := NewBuilder(reg).Build(context.Background(), doc); err == nil {
		t.Fatal("Build unexpectedly succeeded with a typed-nil tool assembly")
	}
}

func assertDefinitions(
	t *testing.T,
	got []string,
	want []string,
	notWant []string,
) {
	t.Helper()
	names := make(map[string]bool, len(got))
	for _, name := range got {
		names[name] = true
	}
	for _, name := range want {
		if !names[name] {
			t.Errorf("definitions missing %q: %v", name, got)
		}
	}
	for _, name := range notWant {
		if names[name] {
			t.Errorf("definitions unexpectedly contain %q: %v", name, got)
		}
	}
}
