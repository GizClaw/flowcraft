package resource_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/agent/scriptrt"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	graphresource "github.com/GizClaw/flowcraft/core/graph/resource"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// echoSource is a host-registered tool.Source with one echo tool.
type echoSource struct{}

func (echoSource) Tools() []tool.Tool {
	return []tool.Tool{tooltest.FuncTool("echo", "echoes",
		func(_ context.Context, args string) (string, error) {
			return "echo:" + args, nil
		})}
}

func (echoSource) LazyTools() []tool.LazyTool { return nil }

type echoSourceFactory struct{}

func (echoSourceFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "tool.Source", Impl: "echo"}
}

func (echoSourceFactory) New(context.Context, resource.Input) (any, error) {
	return echoSource{}, nil
}

// graphDoc is the deployed graph: one JS script node that calls the echo
// tool through the standard `tools` global and records what came back.
const scriptBindingGraph = `{
  "name": "binding-graph",
  "entry": "call",
  "nodes": [
    {
      "id": "call",
      "type": "script",
      "config": {
        "runtime": "js",
        "source": "board.setVar(\"seen\", tools.call(\"echo\", \"{}\").parts[0].text);"
      }
    }
  ],
  "edges": []
}`

// runBindingsDeployment builds dir/deploy.yaml through the real deploy
// pipeline and returns the board value the script wrote.
func runBindingsDeployment(t *testing.T, doc string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "graphs"), 0o755); err != nil {
		t.Fatalf("mkdir graphs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graphs", "main.json"),
		[]byte(scriptBindingGraph), 0o600); err != nil {
		t.Fatalf("write graph: %v", err)
	}

	registry := resource.NewRegistry()
	for _, register := range []func(*resource.Registry) error{
		graphresource.Register, tool.Register, scriptrt.Register,
	} {
		if err := register(registry); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	if err := registry.Register(echoSourceFactory{}); err != nil {
		t.Fatalf("register echo source: %v", err)
	}

	parsed, err := deploy.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	result, err := deploy.NewBuilder(registry,
		deploy.WithLoader(resource.NewLoader(resource.WithBaseDir(dir))),
	).Deploy(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()

	assistant, ok := result.Agent("assistant")
	if !ok {
		t.Fatalf("agents = %v, want assistant", result.AgentNames())
	}
	board := agent.NewBoard()
	if _, err := assistant.Engine.Execute(context.Background(),
		agent.Run{Identity: agent.Identity{AgentID: "assistant", RunID: "run-1"}},
		agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	seen, _ := board.GetVar("seen")
	text, _ := seen.(string)
	return text
}

func bindingsDoc(settings string) string {
	return `version: v1

resources:
  echo:
    kind: tool.Source
    impl: echo
  tools:
    kind: tool.Assembly
    impl: memory
    deps:
      tool: echo
  js:
    kind: agent.ScriptRuntime
    impl: js
  std:
    kind: agent.ScriptBindings
    impl: standard
    deps:
      tools: tools
` + settings + `
agents:
  assistant:
    card:
      name: Assistant
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        tools: tools
        script_runtime: js
        script_bindings: std
      settings:
        graph: {file: ./graphs/main.json}
`
}

func TestDeployDocumentScriptBindingsPolicy(t *testing.T) {
	t.Run("allow list reaches the tools bridge", func(t *testing.T) {
		got := runBindingsDeployment(t, bindingsDoc(`    settings:
      tools:
        allow: [echo]`))
		if got != "echo:{}" {
			t.Fatalf("script saw %q, want the echo tool result", got)
		}
	})

	t.Run("no policy keeps the bridge fail-closed", func(t *testing.T) {
		got := runBindingsDeployment(t, bindingsDoc(""))
		if !strings.Contains(got, "not allowed") {
			t.Fatalf("script saw %q, want a deny result", got)
		}
	})
}

// tokensProvider is a third-party script surface: one pure helper that
// reads per-execution state from the invocation.
type tokensProvider struct{}

func (tokensProvider) Bind(inv bindings.Invocation) ([]bindings.Binding, error) {
	return []bindings.Binding{{
		Name: "tokens",
		Value: map[string]any{
			"estimate": func(text string) int { return len(text) / 4 },
			"nodeID":   func() string { return inv.NodeID },
		},
	}}, nil
}

// tokensBindingsFactory is the host-registered impl: "tokens" extends
// whatever surface its optional "base" dep provides.
type tokensBindingsFactory struct{}

func (tokensBindingsFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: bindings.ResourceKind,
		Impl: "tokens",
		Deps: []resource.DepSpec{{Name: "base", Type: bindings.ResourceKind}},
	}
}

func (tokensBindingsFactory) New(_ context.Context, in resource.Input) (any, error) {
	base, ok := in.Dep("base")
	if !ok {
		return bindings.Provider(tokensProvider{}), nil
	}
	surface, ok := base.(bindings.Provider)
	if !ok {
		return nil, errdefs.Validationf("tokens bindings: base is %T, want bindings.Provider", base)
	}
	return bindings.Chain(surface, tokensProvider{}), nil
}

// hostBindingsGraph uses both surfaces: `board` comes from the base
// (standard) provider, `tokens` from the host provider.
const hostBindingsGraph = `{
  "name": "host-bindings",
  "entry": "compact",
  "nodes": [
    {
      "id": "compact",
      "type": "script",
      "config": {
        "runtime": "js",
        "source": "board.setVar(\"seen\", tokens.estimate(board.getVar(\"transcript\")) + \":\" + tokens.nodeID());"
      }
    }
  ],
  "edges": []
}`

func TestDeployDocumentHostBindingsComposeWithStandard(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "graphs"), 0o755); err != nil {
		t.Fatalf("mkdir graphs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graphs", "main.json"),
		[]byte(hostBindingsGraph), 0o600); err != nil {
		t.Fatalf("write graph: %v", err)
	}

	registry := resource.NewRegistry()
	for _, register := range []func(*resource.Registry) error{
		graphresource.Register, tool.Register, scriptrt.Register,
	} {
		if err := register(registry); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	if err := registry.Register(echoSourceFactory{}); err != nil {
		t.Fatalf("register echo source: %v", err)
	}
	if err := registry.Register(tokensBindingsFactory{}); err != nil {
		t.Fatalf("register tokens bindings: %v", err)
	}

	doc := `version: v1

resources:
  echo:
    kind: tool.Source
    impl: echo
  tools:
    kind: tool.Assembly
    impl: memory
    deps:
      tool: echo
  js:
    kind: agent.ScriptRuntime
    impl: js
  std:
    kind: agent.ScriptBindings
    impl: standard
    deps:
      tools: tools
    settings:
      tools: {allow: [echo]}
  tok:
    kind: agent.ScriptBindings
    impl: tokens
    deps:
      base: std

agents:
  assistant:
    card:
      name: Assistant
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        tools: tools
        script_runtime: js
        script_bindings: tok
      settings:
        graph: {file: ./graphs/main.json}
`
	parsed, err := deploy.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	result, err := deploy.NewBuilder(registry,
		deploy.WithLoader(resource.NewLoader(resource.WithBaseDir(dir))),
	).Deploy(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = result.Close() }()

	assistant, ok := result.Agent("assistant")
	if !ok {
		t.Fatalf("agents = %v, want assistant", result.AgentNames())
	}
	board := agent.NewBoard()
	board.SetVar("transcript", "0123456789abcdef")
	if _, err := assistant.Engine.Execute(context.Background(),
		agent.Run{Identity: agent.Identity{AgentID: "assistant", RunID: "run-1"}},
		agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	seen, _ := board.GetVar("seen")
	if seen != "4:compact" {
		t.Fatalf("board.seen = %v, want the composed surface output", seen)
	}
}
