package yaml

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	memoryconfig "github.com/GizClaw/flowcraft/sdkx/memory/config"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestResourceSpec(t *testing.T) {
	spec := NewDeployFactory().Spec()
	if spec.Kind != "memory.Assembly" || spec.ItemType != "memory.System" || len(spec.Deps) != 2 {
		t.Fatalf("Spec = %+v", spec)
	}
	want := map[string]string{
		"workspace": "workspace.Workspace",
		"inference": "inference.Runtime",
	}
	for _, dep := range spec.Deps {
		if !dep.Required || want[dep.Name] != dep.Type {
			t.Fatalf("dependency = %+v", dep)
		}
	}
}

func TestTypedDAGYAMLRoundTripAndNestedUnknownField(t *testing.T) {
	source := `
scopes:
  - runtime_id: memory
chat_dag:
  nodes:
    - id: facts
      fact:
        strategy: simple
knowledge_dag:
  nodes:
    - id: chunks
      chunk:
        max_runes: 256
        overlap_runes: 16
lifecycle_dag:
  nodes:
    - id: integrate
      phase: integrate
    - id: repair
      phase: repair
      depends_on: [integrate]
algorithm_catalog:
  - name: custom.example
    version: 2.0.0
`
	settings, err := deploy.DecodeSettings[memoryconfig.Settings](node(t, source))
	if err != nil {
		t.Fatal(err)
	}
	data, err := yamlv3.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := deploy.DecodeSettings[memoryconfig.Settings](node(t, string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(settings, roundTrip) {
		t.Fatalf("round trip changed settings:\nfirst=%#v\nsecond=%#v", settings, roundTrip)
	}

	_, err = deploy.DecodeSettings[memoryconfig.Settings](node(t, `
chat_dag:
  nodes:
    - id: facts
      fact:
        arbitrary_config: true
`))
	if err == nil {
		t.Fatal("accepted unknown arbitrary algorithm config")
	}

	unknown, err := deploy.DecodeSettings[memoryconfig.Settings](node(t, `
lifecycle_dag:
  nodes:
    - id: unknown
      phase: arbitrary
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memoryconfig.ComputePolicyDigest(unknown); err == nil {
		t.Fatal("accepted unknown lifecycle built-in")
	}

	_, err = deploy.DecodeSettings[memoryconfig.Settings](node(t, `
lifecycle_dag:
  nodes:
    - id: custom
      custom:
        factory: arbitrary
`))
	if err == nil {
		t.Fatal("accepted programmatic custom lifecycle factory in YAML")
	}
}

func TestResourceStrictDecodeAndDependencyTypes(t *testing.T) {
	factory := NewDeployFactory()
	_, err := factory.New(context.Background(), deploy.ResourceInput{
		Settings: node(t, "unknown: true\n"),
		Deps: map[string]any{
			"workspace": workspace.NewMemWorkspace(),
			"inference": &inference.Runtime{},
		},
	})
	if err == nil {
		t.Fatal("accepted unknown setting")
	}

	for name, deps := range map[string]map[string]any{
		"workspace": {"workspace": "wrong", "inference": &inference.Runtime{}},
		"inference": {"workspace": workspace.NewMemWorkspace(), "inference": "wrong"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := factory.New(context.Background(), deploy.ResourceInput{Deps: deps})
			if err == nil {
				t.Fatalf("accepted dependency types %v", reflect.TypeOf(deps[name]))
			}
		})
	}
}

func node(t *testing.T, source string) *yamlv3.Node {
	t.Helper()
	var document yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(source), &document); err != nil {
		t.Fatal(err)
	}
	return document.Content[0]
}
