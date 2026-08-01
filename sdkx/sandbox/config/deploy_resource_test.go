package config_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	sandboxconfig "github.com/GizClaw/flowcraft/sdkx/sandbox/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdkx/workspace/config"
)

type resourceEngineFactory struct {
	got agent.Config
}

func (f *resourceEngineFactory) Spec() agent.EngineSpec {
	return agent.EngineSpec{
		Kind: "resource-test",
		Deps: []agent.DepSpec{
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
			{Name: "runner", Type: "sandbox.Runner", Required: true},
		},
	}
}

func (f *resourceEngineFactory) New(_ context.Context, config agent.Config) (agent.Engine, error) {
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

// TestRegistriesWireAsDeployResources is the end-to-end shape of the
// resource area: one pure-YAML document declares workspaces and
// sandboxes, the sandbox resource reaches its workspaces through deps,
// and agent deps bind single items out of both containers.
func TestRegistriesWireAsDeployResources(t *testing.T) {
	engineRegistry := agent.NewRegistry()
	factory := &resourceEngineFactory{}
	if err := engineRegistry.Register(factory); err != nil {
		t.Fatal(err)
	}

	builder := deploy.NewBuilder(engineRegistry)
	builder.RegisterResource(
		workspaceconfig.ResourceKind, "yaml", workspaceconfig.DeployResource)
	builder.RegisterResource(
		sandboxconfig.ResourceKind, "yaml", sandboxconfig.DeployResource)

	// "boxes" sorts before "files" so a lexical build order would try
	// to construct the sandbox registry before its workspaces exist.
	doc, err := deploy.Parse([]byte(`
version: v1
resources:
  boxes:
    kind: sandbox.Registry
    impl: yaml
    deps: {workspaces: files}
    settings:
      inline:
        version: v1
        sandboxes:
          coding:
            backend: local
            workspace: project
  files:
    kind: workspace.Registry
    impl: yaml
    settings:
      inline:
        version: v1
        workspaces:
          project:
            driver: local
            settings: {root: project}
agents:
  coder:
    engine: {kind: resource-test}
    deps:
      workspace: files/project
      runner: boxes/coding
`))
	if err != nil {
		t.Fatal(err)
	}

	result, err := builder.Build(context.Background(), doc)
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

// TestSandboxResourceRejectsWrongWorkspacesDep pins the type assertion
// in the adapter: deps are resolved by name, so a document binding the
// wrong resource kind must fail at build time rather than at the first
// command.
func TestSandboxResourceRejectsWrongWorkspacesDep(t *testing.T) {
	_, err := sandboxconfig.DeployResource(context.Background(), deploy.ResourceInput{
		Deps: map[string]any{sandboxconfig.WorkspacesDep: "not a registry"},
	})
	if err == nil {
		t.Fatal("want error for a non-Registry workspaces dep")
	}
}
