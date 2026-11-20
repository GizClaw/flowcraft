package deploy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
)

// deployLayer is a deployment document as one layer of a stack: the shape a host
// ships as a default, a project file or a user override.
func deployLayer(priority int, name, body string) config.Layer {
	return config.Layer{Priority: priority, Name: name, Source: config.Bytes([]byte(body))}
}

// memoryLayers is the source table these tests read runtime layers from, keyed by
// the name a document writes.
func memoryLayers(values map[string]string) deploy.Kinds {
	return localKinds(memoryKind(values))
}

// One layer is the same document Load reads, so the two entries must agree: a
// layered load may not be a second, looser rule about what a document means.
func TestLoadLayersMatchesLoad(t *testing.T) {
	kinds := memoryLayers(map[string]string{"base": layerConfig("from-memory")})
	document := layerDocument("- priority: 0\n  source: {kind: memory, settings: {name: base}}\n")

	single, err := deploy.Loader{Kinds: kinds}.Load(context.Background(), document)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	layered, err := deploy.Loader{Kinds: kinds}.LoadLayers(context.Background(),
		[]config.Layer{deployLayer(0, "deploy.yaml", string(document))})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if len(single) != len(layered) {
		t.Fatalf("specs = %d from one body, %d from one layer", len(single), len(layered))
	}
	if single[0].Name != layered[0].Name || single[0].Engine.Kind != layered[0].Engine.Kind {
		t.Errorf("spec = %+v, want the same as Load's %+v", layered[0], single[0])
	}
	if single[0].Runtime.Name != layered[0].Runtime.Name {
		t.Errorf("runtime name = %q, want Load's %q", layered[0].Runtime.Name, single[0].Runtime.Name)
	}
}

// The priority orders the stack, not the order the layers arrive in: a host that
// appends a directory scan after a command-line profile still gets the profile on
// top.
func TestLoadLayersAppliesPriorityNotOrder(t *testing.T) {
	kinds := memoryLayers(map[string]string{
		"defaults": layerConfig("from-defaults"),
		"override": layerConfig("from-override"),
	})
	defaults := layerDocument("- priority: 0\n  source: {kind: memory, settings: {name: defaults}}\n")
	override := layerDocument("- priority: 0\n  source: {kind: memory, settings: {name: override}}\n")

	specs, err := deploy.Loader{Kinds: kinds}.LoadLayers(context.Background(), []config.Layer{
		deployLayer(10, "user.yaml", string(override)),
		deployLayer(0, "defaults.yaml", string(defaults)),
	})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if got := specs[0].Runtime.Name; got != "from-override" {
		t.Errorf("runtime name = %q, want the higher-priority layer's", got)
	}
}

// A sequence is replaced whole, so `agents` is not merged element by element: an
// agent the higher layer does not repeat is gone, and one it does repeat does not
// inherit the lower layer's fields. That is core/config's rule, and the loader
// docs say so rather than inventing a second one here.
func TestLoadLayersReplacesTheAgentList(t *testing.T) {
	kinds := memoryLayers(map[string]string{"base": layerConfig("from-memory")})
	defaults := `version: 1
agents:
  - name: forge
    engine: {kind: local}
    runtime:
      layers:
        - priority: 0
          source: {kind: memory, settings: {name: base}}
    catalog: {scope: session}
  - name: reviewer
    engine: {kind: local}
    runtime:
      layers:
        - priority: 0
          source: {kind: memory, settings: {name: base}}
`
	override := `version: 1
agents:
  - name: forge
    engine: {kind: local}
    runtime:
      layers:
        - priority: 0
          source: {kind: memory, settings: {name: base}}
`
	specs, err := deploy.Loader{Kinds: kinds}.LoadLayers(context.Background(), []config.Layer{
		deployLayer(0, "defaults.yaml", defaults),
		deployLayer(10, "user.yaml", override),
	})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "forge" {
		t.Fatalf("specs = %+v, want only the agent the higher layer writes", specs)
	}
	if specs[0].Catalog.Scope != "" {
		t.Errorf("catalog scope = %q, want the lower layer's scope to be gone with its list",
			specs[0].Catalog.Scope)
	}
}

// Placeholders are expanded once, after the merge, so a resolver that returns a
// reference hands it back as text: a host that merged the document itself and
// re-loaded it would expand the value a second time, which is what this entry
// exists to avoid.
func TestLoadLayersExpandsPlaceholdersOnce(t *testing.T) {
	t.Setenv("TOKEN", "s3cr3t")
	kinds := memoryLayers(map[string]string{"base": layerConfig("from-memory")})
	loader := deploy.Loader{
		Kinds: kinds,
		LoadOptions: []config.Option{config.WithResolver("stable",
			func(context.Context, []string) (string, error) { return "${env:TOKEN}", nil })},
	}
	document := []byte(`version: 1
agents:
  - name: local
    engine: {kind: local, settings: {marker: "${stable:any}"}}
    runtime:
      layers:
        - priority: 0
          source: {kind: memory, settings: {name: base}}
`)
	specs, err := loader.LoadLayers(context.Background(),
		[]config.Layer{deployLayer(0, "deploy.yaml", string(document))})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	settings := string(specs[0].Engine.Settings)
	if !strings.Contains(settings, "${env:TOKEN}") {
		t.Errorf("settings = %s, want the resolved reference kept as text", settings)
	}
	if strings.Contains(settings, "s3cr3t") {
		t.Errorf("settings = %s, want the resolved value not expanded again", settings)
	}
}

// Expansion runs after the merge, not per layer: a value the winning layer
// overrides is never looked up, so a dangling reference in a losing layer is not
// an error.
func TestLoadLayersExpandsOnlyTheWinningValue(t *testing.T) {
	kinds := memoryLayers(map[string]string{"base": layerConfig("from-memory")})
	lower := `version: 1
agents:
  - name: local
    engine: {kind: local, settings: {marker: "${FLOWCRAFT_TEST_UNSET_PLACEHOLDER}"}}
    runtime:
      layers:
        - priority: 0
          source: {kind: memory, settings: {name: base}}
`
	upper := layerDocument("- priority: 0\n  source: {kind: memory, settings: {name: base}}\n")
	_, err := deploy.Loader{Kinds: kinds}.LoadLayers(context.Background(), []config.Layer{
		deployLayer(0, "defaults.yaml", lower),
		deployLayer(10, "user.yaml", string(upper)),
	})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
}

// The merged document is validated, not each layer: a higher layer that changes
// the version is a document error, named the way Load names it.
func TestLoadLayersValidatesTheMergedDocument(t *testing.T) {
	kinds := memoryLayers(map[string]string{"base": layerConfig("from-memory")})
	document := layerDocument("- priority: 0\n  source: {kind: memory, settings: {name: base}}\n")

	_, err := deploy.Loader{Kinds: kinds}.LoadLayers(context.Background(), []config.Layer{
		deployLayer(0, "defaults.yaml", string(document)),
		deployLayer(10, "user.yaml", "version: 2\n"),
	})
	if err == nil {
		t.Fatal("LoadLayers accepted a merged document with version 2")
	}
	if !strings.Contains(err.Error(), "version 2 is not supported") {
		t.Errorf("error = %v, want the version rule", err)
	}
}

// A stack with no layers describes no document, the same rule config.Load
// applies to a runtime.
func TestLoadLayersRequiresALayer(t *testing.T) {
	_, err := deploy.Loader{Kinds: memoryLayers(nil)}.LoadLayers(context.Background(), nil)
	if err == nil {
		t.Fatal("LoadLayers accepted an empty stack")
	}
	if !strings.Contains(err.Error(), "at least one layer is required") {
		t.Errorf("error = %v, want the empty-stack rule", err)
	}
}

// Two layers at one priority have no defined winner, so the stack is refused
// instead of being broken by a tie-break nobody can predict.
func TestLoadLayersRejectsDuplicatePriority(t *testing.T) {
	kinds := memoryLayers(map[string]string{"base": layerConfig("from-memory")})
	document := string(layerDocument("- priority: 0\n  source: {kind: memory, settings: {name: base}}\n"))
	_, err := deploy.Loader{Kinds: kinds}.LoadLayers(context.Background(), []config.Layer{
		deployLayer(0, "defaults.yaml", document),
		deployLayer(0, "user.yaml", document),
	})
	if err == nil {
		t.Fatal("LoadLayers accepted two layers at one priority")
	}
	for _, want := range []string{"share priority", "defaults.yaml", "user.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A layer that cannot be parsed is named: the stack, not the file, is what the
// reader has to open.
func TestLoadLayersNamesTheLayerThatFailed(t *testing.T) {
	_, err := deploy.Loader{Kinds: memoryLayers(nil)}.LoadLayers(context.Background(),
		[]config.Layer{deployLayer(0, "user.yaml", "agents: [\n")})
	if err == nil {
		t.Fatal("LoadLayers accepted an unparseable layer")
	}
	if !strings.Contains(err.Error(), "user.yaml") {
		t.Errorf("error %q does not name the layer", err)
	}
}

// Layering the deployment document does not remove the runtime stack inside it:
// once a layer wins an agent, its runtime layers are read through the source
// table exactly as they are for Load.
func TestLoadLayersKeepsTheRuntimeStack(t *testing.T) {
	kinds := memoryLayers(map[string]string{
		"base": layerConfig("from-base"),
		"prod": layerConfig("from-prod"),
	})
	document := layerDocument(
		"- priority: 0\n  source: {kind: memory, settings: {name: base}}\n" +
			"- priority: 10\n  source: {kind: memory, settings: {name: prod}}\n")
	specs, err := deploy.Loader{Kinds: kinds}.LoadLayers(context.Background(),
		[]config.Layer{deployLayer(0, "deploy.yaml", string(document))})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if got := specs[0].Runtime.Name; got != "from-prod" {
		t.Errorf("runtime name = %q, want the higher runtime layer", got)
	}
}
