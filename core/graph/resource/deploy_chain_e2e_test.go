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

// chainToolSource exposes echo and other, so the chain test can prove
// the base's allow-list still governs the composed surface.
type chainToolSource struct{}

func (chainToolSource) Tools() []tool.Tool {
	return []tool.Tool{
		tooltest.FuncTool("echo", "echoes", func(_ context.Context, args string) (string, error) {
			return "echo:" + args, nil
		}),
		tooltest.FuncTool("other", "other", func(context.Context, string) (string, error) {
			return "other", nil
		}),
	}
}

func (chainToolSource) LazyTools() []tool.LazyTool { return nil }

type chainToolSourceFactory struct{}

func (chainToolSourceFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "tool.Source", Impl: "both"}
}

func (chainToolSourceFactory) New(context.Context, resource.Input) (any, error) {
	return chainToolSource{}, nil
}

// auditProvider binds its global only in the late phase, proving late
// units run through the chain.
type auditProvider struct{}

func (auditProvider) Bind(bindings.Invocation) ([]bindings.Binding, error) { return nil, nil }

func (auditProvider) BindLate(bindings.Invocation, *agent.ScriptEnv) ([]bindings.Binding, error) {
	return []bindings.Binding{{Name: "audit", Value: map[string]any{
		"stamp": func() string { return "late" },
	}}}, nil
}

type auditBindingsFactory struct{}

func (auditBindingsFactory) Spec() resource.Spec {
	return resource.Spec{Kind: bindings.ResourceKind, Impl: "audit"}
}

func (auditBindingsFactory) New(context.Context, resource.Input) (any, error) {
	return auditProvider{}, nil
}

// clashProvider binds one global; two of them collide, and the chain's
// duplicate-name report is what the negative test pins down.
type clashProvider struct{}

func (clashProvider) Bind(bindings.Invocation) ([]bindings.Binding, error) {
	return []bindings.Binding{{Name: "clash", Value: "x"}}, nil
}

type clashBindingsFactory struct{}

func (clashBindingsFactory) Spec() resource.Spec {
	return resource.Spec{Kind: bindings.ResourceKind, Impl: "clash"}
}

func (clashBindingsFactory) New(context.Context, resource.Input) (any, error) {
	return clashProvider{}, nil
}

// chainGraph drives the composed surface: board and tools from the
// standard base, tokens from a host unit, audit from a late host unit.
const chainGraph = `{
  "name": "chain-graph",
  "entry": "run",
  "nodes": [
    {
      "id": "run",
      "type": "script",
      "config": {
        "runtime": "js",
        "source": "var denied = tools.call(\"other\", \"{}\"); board.setVar(\"seen\", tools.call(\"echo\", \"{}\").parts[0].text + \"|\" + tokens.estimate(\"0123456789abcdef\") + \"|\" + audit.stamp() + \"|\" + denied.is_error);"
      }
    }
  ],
  "edges": []
}`

// clashGraph never runs its script: the duplicate global fails the
// invocation's assembly first.
const clashGraph = `{
  "name": "clash-graph",
  "entry": "run",
  "nodes": [
    {
      "id": "run",
      "type": "script",
      "config": {
        "runtime": "js",
        "source": "board.setVar(\"seen\", \"ran\");"
      }
    }
  ],
  "edges": []
}`

// sameResourceDoc mounts one standard resource as both the base and a
// unit: the kind's own impls are struct values, so the build cannot see
// the double mount and the deployment succeeds.
const sameResourceDoc = `version: v1

resources:
  js:
    kind: agent.ScriptRuntime
    impl: js
  script:
    kind: graph.NodeType
    impl: script
    deps:
      script_runtime.js: js
  std:
    kind: agent.ScriptBindings
    impl: standard
  panel:
    kind: agent.ScriptBindings
    impl: chain
    deps: {base: std, unit.same: std}

agents:
  assistant:
    card:
      name: Assistant
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        node_type.script: script
        script_bindings: panel
      settings:
        graph: {file: ./graphs/main.json}
`

// runChainDeployment builds dir/deploy.yaml through the real deploy
// pipeline, then executes it and returns the board value the script
// wrote together with the Execute error.
func runChainDeployment(t *testing.T, doc, graph string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "graphs"), 0o755); err != nil {
		t.Fatalf("mkdir graphs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graphs", "main.json"), []byte(graph), 0o600); err != nil {
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
	for _, factory := range []resource.Factory[any]{
		chainToolSourceFactory{}, tokensBindingsFactory{},
		auditBindingsFactory{}, clashBindingsFactory{},
	} {
		if err := resource.Register(registry, factory); err != nil {
			t.Fatalf("register %T: %v", factory, err)
		}
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
		return "", err
	}
	seen, _ := board.Var("seen")
	text, _ := seen.(string)
	return text, nil
}

// chainDoc composes the standard surface with a plain host unit and a
// late host unit through one chain resource; the engine consumes the
// chain like any other provider.
const chainDoc = `version: v1

resources:
  echo:
    kind: tool.Source
    impl: both
  tools:
    kind: tool.Assembly
    impl: memory
    deps:
      tool: echo
  js:
    kind: agent.ScriptRuntime
    impl: js
  script:
    kind: graph.NodeType
    impl: script
    deps:
      script_runtime.js: js
      tools: tools
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
  audit:
    kind: agent.ScriptBindings
    impl: audit
  panel:
    kind: agent.ScriptBindings
    impl: chain
    deps: {base: std, unit.audit: audit, unit.tok: tok}

agents:
  assistant:
    card:
      name: Assistant
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        node_type.script: script
        script_bindings: panel
      settings:
        graph: {file: ./graphs/main.json}
`

// clashDoc mounts two units that both bind "clash": the document
// deploys, and the duplicate only surfaces when the script runs.
const clashDoc = `version: v1

resources:
  js:
    kind: agent.ScriptRuntime
    impl: js
  script:
    kind: graph.NodeType
    impl: script
    deps:
      script_runtime.js: js
  left:
    kind: agent.ScriptBindings
    impl: clash
  right:
    kind: agent.ScriptBindings
    impl: clash
  panel:
    kind: agent.ScriptBindings
    impl: chain
    deps: {unit.left: left, unit.right: right}

agents:
  assistant:
    card:
      name: Assistant
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        node_type.script: script
        script_bindings: panel
      settings:
        graph: {file: ./graphs/main.json}
`

func TestDeployDocumentScriptBindingsChain(t *testing.T) {
	seen, err := runChainDeployment(t, chainDoc, chainGraph)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// echo:{} proves the base's allow-list admits the tool through the
	// chain; tokens.estimate runs from a unit without a base; audit
	// comes from a late unit; other stays denied by the base's policy.
	want := "echo:{}|4|late|true"
	if seen != want {
		t.Fatalf("board.seen = %q, want %q", seen, want)
	}
}

func TestDeployDocumentScriptBindingsChainDuplicateGlobal(t *testing.T) {
	_, err := runChainDeployment(t, clashDoc, clashGraph)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Execute = %v, want Validation for the duplicate global", err)
	}
	for _, want := range []string{`global "clash"`, `"unit.left"`, `"unit.right"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v must contain %s", err, want)
		}
	}
}

func TestDeployDocumentScriptBindingsChainSharedStandardResource(t *testing.T) {
	// The build-time guard is best-effort — it identifies pointer-backed
	// values only — so one standard resource under two keys deploys, and
	// the duplicate global fails the execution naming both dep keys.
	_, err := runChainDeployment(t, sameResourceDoc, clashGraph)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Execute = %v, want Validation for the duplicate global", err)
	}
	for _, want := range []string{`global "board"`, `"base"`, `"unit.same"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v must contain %s", err, want)
		}
	}
}
