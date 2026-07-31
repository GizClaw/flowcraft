package config_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	agentconfig "github.com/GizClaw/flowcraft/sdkx/agent/config"
	sandboxconfig "github.com/GizClaw/flowcraft/sdkx/sandbox/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdkx/workspace/config"
)

type sourceEngineFactory struct {
	got agent.Config
}

func (f *sourceEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{
		Kind: "source-test",
		Deps: []agent.DepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
			{Name: "runner", Type: "sandbox.Runner", Required: true},
		},
	}
}

func (f *sourceEngineFactory) New(_ context.Context, config agent.Config) (agent.Engine, error) {
	f.got = config
	return agent.EngineFunc(func(
		context.Context,
		agent.Run,
		agent.Host,
		*agent.Board,
	) (*agent.Board, error) {
		return agent.NewBoard(), nil
	}), nil
}

func TestWorkspaceAndSandboxRegistriesWireAsAgentSources(t *testing.T) {
	workspaceDoc, err := workspaceconfig.Parse([]byte(`
version: v1
workspaces:
  project:
    driver: local
    settings: {root: project}
`))
	if err != nil {
		t.Fatal(err)
	}
	workspaces, err := workspaceconfig.NewBuilder(workspaceconfig.Deps{
		BaseDir: t.TempDir(),
	}).Build(context.Background(), workspaceDoc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = workspaces.Close() }()

	sandboxDoc, err := sandboxconfig.Parse([]byte(`
version: v1
sandboxes:
  coding:
    backend: local
    workspace: project
`))
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: workspaces,
	}).Build(context.Background(), sandboxDoc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sandboxes.Close() }()

	engineRegistry := agent.NewRegistry()
	factory := &sourceEngineFactory{}
	if err := engineRegistry.Register(factory); err != nil {
		t.Fatal(err)
	}
	builder := agentconfig.NewBuilder(engineRegistry)
	builder.RegisterSource("workspace", workspaces.Resolve)
	builder.RegisterSource("sandbox", sandboxes.Resolve)

	agentDoc, err := agentconfig.Parse([]byte(`
version: v1
agents:
  coder:
    engine: {kind: source-test}
    deps:
      workspace: {source: workspace, ref: project}
      runner: {source: sandbox, ref: coding}
`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(context.Background(), agentDoc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = result.Close() }()

	if _, ok := factory.got.Deps["workspace"].(workspace.Workspace); !ok {
		t.Fatalf("workspace dep = %T, want workspace.Workspace",
			factory.got.Deps["workspace"])
	}
	if _, ok := factory.got.Deps["runner"].(coresandbox.Runner); !ok {
		t.Fatalf("runner dep = %T, want sandbox.Runner",
			factory.got.Deps["runner"])
	}
}
