package config_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	sandboxconfig "github.com/GizClaw/flowcraft/sdkx/sandbox/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdkx/workspace/config"
	yamlv3 "gopkg.in/yaml.v3"
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
	if err := builder.RegisterResource(workspaceconfig.NewDeployFactory()); err != nil {
		t.Fatal(err)
	}
	if err := builder.RegisterResource(sandboxconfig.NewDeployFactory()); err != nil {
		t.Fatal(err)
	}

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

	if _, err := deploy.ResourceAs[*workspaceconfig.Registry](result, "files"); err != nil {
		t.Fatalf("ResourceAs(files): %v", err)
	}
	if _, err := deploy.ResourceAs[*sandboxconfig.Registry](result, "boxes"); err != nil {
		t.Fatalf("ResourceAs(boxes): %v", err)
	}
	if _, ok := factory.got.Deps["workspace"].(workspace.Workspace); !ok {
		t.Fatalf("workspace dep = %T, want workspace.Workspace",
			factory.got.Deps["workspace"])
	}
	if _, ok := factory.got.Deps["runner"].(coresandbox.Runner); !ok {
		t.Fatalf("runner dep = %T, want sandbox.Runner",
			factory.got.Deps["runner"])
	}
}

func TestDeployFactorySpec(t *testing.T) {
	got := sandboxconfig.NewDeployFactory().Spec()
	want := deploy.ResourceSpec{
		Kind: sandboxconfig.ResourceKind,
		Impl: "yaml",
		Deps: []deploy.ResourceDepSpec{{
			Name:     sandboxconfig.WorkspacesDep,
			Type:     workspaceconfig.ResourceKind,
			Required: true,
		}},
		ItemType: "sandbox.Runner",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewRejectsInvalidDependenciesAndSettings(t *testing.T) {
	factory := sandboxconfig.NewDeployFactory()
	settings := sandboxSettingsNode(t, `
inline:
  version: v1
  sandboxes: {}
`)
	for name, deps := range map[string]map[string]any{
		"missing": nil,
		"wrong type": {
			sandboxconfig.WorkspacesDep: "not a registry",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := factory.New(context.Background(), deploy.ResourceInput{
				Settings: settings,
				Deps:     deps,
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("New error = %v, want validation", err)
			}
		})
	}
	if _, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: sandboxSettingsNode(t, "unknown: true\n"),
		Deps: map[string]any{
			sandboxconfig.WorkspacesDep: (*workspaceconfig.Registry)(nil),
		},
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
}

func sandboxSettingsNode(t *testing.T, input string) *yamlv3.Node {
	t.Helper()
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(input), &node); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return node.Content[0]
}
