package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/builtin"
	enginesgraph "github.com/GizClaw/flowcraft/core/deploy/engines/graph"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// fixedKind writes one text value, so a test graph needs no model.
type fixedKind struct{ text string }

func (fixedKind) Name() string { return "fixed" }

func (k fixedKind) Invoke(context.Context, graph.Invoke) (graph.NodeStream, error) {
	return graph.Static(graph.Result{State: []graph.StateUpdate{
		{Key: "text", Value: graph.Text(k.text)},
	}}), nil
}

// configKind writes the text its config names, which is how a test observes a
// value the document itself carries - a placeholder the loader expanded, as
// opposed to one the test handed the graph.
type configKind struct{}

func (configKind) Name() string { return "configured" }

func (configKind) Invoke(_ context.Context, in graph.Invoke) (graph.NodeStream, error) {
	var config struct {
		Text string `json:"text"`
	}
	if err := graph.DecodeConfig(in.Config, &config); err != nil {
		return nil, err
	}
	return graph.Static(graph.Result{State: []graph.StateUpdate{
		{Key: "text", Value: graph.Text(config.Text)},
	}}), nil
}

// configuredGraph is fixedGraph with the value in the node's own config, so a
// placeholder has to survive expansion to reach the state.
const configuredGraph = `version: 1
entry: a
state:
  text: {type: text}
nodes:
  - {id: a, kind: configured, config: {text: "${demo:greeting}"}}
edges:
  - {from: a, to: end}
output: {key: text}
`

// demoResolvers is the host's registration for the scheme these tests read.
func demoResolvers() []config.Option {
	return []config.Option{config.WithResolver("demo",
		func(context.Context, []string) (string, error) { return "from-the-host", nil })}
}

func writeGraphDocument(t *testing.T, body string) string {
	t.Helper()
	return writeTempYAML(t, "graph.yaml", body)
}

// writeTempYAML is one document on disk, in its own directory.
func writeTempYAML(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

const fixedGraph = `version: 1
entry: a
state:
  text: {type: text}
nodes:
  - {id: a, kind: fixed}
edges:
  - {from: a, to: end}
output: {key: text}
`

// The graph engine is a real deployment: a document on disk, compiled per
// deployment, driven through the same session a local engine uses.
func TestGraphEngineRunsADocument(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(enginesgraph.Engine(nodes.WithKinds(fixedKind{text: "hello"})))
	manager := newManager(t, kinds, testHost(t))
	spec := testSpec(t, "g", "graph")
	spec.Engine.Settings = graphSettingsFor(t, writeGraphDocument(t, fixedGraph))
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := turn(t, manager, "g"); got != "hello" {
		t.Errorf("text = %q, want the graph's output", got)
	}
}

// A node config is expanded by the same code a runtime layer is, so a document may
// name a scheme the host registered. The options are the engine's, because the
// loader validates the document through EngineKind.Decode before any EngineDeps
// exist: this is the half of the wiring a host gets wrong first.
func TestGraphDocumentResolvesAHostSchemeAtLoad(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(enginesgraph.Engine(
		nodes.WithKinds(configKind{}),
		nodes.WithConfigOptions(demoResolvers()...),
	))
	document := writeDeploymentDocument(t,
		map[string]any{"kind": "file", "settings": map[string]any{
			"path": writeGraphDocument(t, configuredGraph)}},
		map[string]any{"kind": "file", "settings": map[string]any{"path": writeRuntimeLayer(t)}},
	)
	if got := runDeploymentDocument(t, kinds, document); got != "from-the-host" {
		t.Errorf("text = %q, want the resolved value", got)
	}
}

// The build half: a Spec composed in code never passes through the loader, so the
// host's resolvers reach the graph as EngineDeps.LoadOptions instead.
func TestGraphEngineUsesHostLoadOptionsAtBuild(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(enginesgraph.Engine(nodes.WithKinds(configKind{})))
	bindings := testHost(t)
	bindings.LoadOptions = demoResolvers()
	manager := newManager(t, kinds, bindings)
	spec := testSpec(t, "g", "graph")
	spec.Engine.Settings = graphSettingsFor(t, writeGraphDocument(t, configuredGraph))
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := turn(t, manager, "g"); got != "from-the-host" {
		t.Errorf("text = %q, want the resolved value", got)
	}
}

// Without either half the scheme is unknown, and the failure names the document and
// the schemes it could have used rather than handing a literal to a node.
func TestGraphEngineRefusesAHostSchemeWithoutOptions(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(enginesgraph.Engine(nodes.WithKinds(configKind{})))
	manager := newManager(t, kinds, testHost(t))
	spec := testSpec(t, "g", "graph")
	spec.Engine.Settings = graphSettingsFor(t, writeGraphDocument(t, configuredGraph))
	err := manager.Add(context.Background(), spec)
	if err == nil {
		t.Fatal("a document naming an unregistered scheme was added")
	}
	for _, want := range []string{`unknown placeholder scheme "demo"`, "offered: env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// graphSettingsFor is the engine settings a document on disk is named by.
func graphSettingsFor(t *testing.T, path string) json.RawMessage {
	t.Helper()
	return rawSettings(map[string]any{
		"source": map[string]any{
			"kind":     "file",
			"settings": map[string]any{"path": path},
		},
	})
}

// turn drives one turn of a deployed agent and returns the text it streamed.
func turn(t *testing.T, manager *Manager, name string) string {
	t.Helper()
	session, ok := manager.Session(name, "conv-1")
	if !ok {
		t.Fatalf("no session for %q", name)
	}
	var text string
	for event, err := range session.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if typed, ok := event.(agent.Text); ok {
			text += typed.Text
		}
	}
	return text
}

// The whole path in one run, over both kinds of address: a document that names
// the file source and one that names a source the host registered. The second is
// the case the feature exists for, and the only test in which one host table
// serves a runtime layer and a graph document at once.
func TestDeploymentDocumentRunsEndToEnd(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(enginesgraph.Engine(nodes.WithKinds(fixedKind{text: "hello"})))
	layer := writeRuntimeLayer(t)
	graphDocument := writeGraphDocument(t, fixedGraph)
	document := writeDeploymentDocument(t,
		map[string]any{"kind": "file", "settings": map[string]any{"path": graphDocument}},
		map[string]any{"kind": "file", "settings": map[string]any{"path": layer}},
	)
	if got := runDeploymentDocument(t, kinds, document); got != "hello" {
		t.Errorf("text = %q, want the graph's output", got)
	}
}

// The host-value case: one registration serves both documents, which is what the
// source table promises and no per-package test can show.
func TestHostSourceDeploymentRunsEndToEnd(t *testing.T) {
	tree := fstest.MapFS{
		"runtime.yaml": {Data: []byte(runtimeLayerBody(t))},
		"graph.yaml":   {Data: []byte(fixedGraph)},
	}
	kinds := kindsWithHostSource(tree)
	document := writeDeploymentDocument(t,
		map[string]any{"kind": "assets", "settings": map[string]any{"name": "graph.yaml"}},
		map[string]any{"kind": "assets", "settings": map[string]any{"name": "runtime.yaml"}},
	)
	if got := runDeploymentDocument(t, kinds, document); got != "hello" {
		t.Errorf("text = %q, want the graph's output", got)
	}
}

// kindsWithHostSource is the deployment's kind table with one host-registered
// source under a name the host chose.
func kindsWithHostSource(tree fstest.MapFS) deploy.Kinds {
	// `file` is registered beside `assets` because the deployment document itself
	// is still read from disk: LoadFile is the file source written as a call.
	return deploy.Kinds{}.
		WithEngines(enginesgraph.Engine(nodes.WithKinds(fixedKind{text: "hello"}))).
		WithSources(sources.File(), sources.FS("assets", tree))
}

// writeDeploymentDocument writes the document as JSON: config reads it as YAML,
// and a JSON string needs no quoting decisions, so a Windows path survives.
func writeDeploymentDocument(t *testing.T, engine, layer map[string]any) string {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"version": 1,
		"agents": []any{map[string]any{
			"name":   "g",
			"engine": map[string]any{"kind": "graph", "settings": map[string]any{"source": engine}},
			"runtime": map[string]any{"layers": []any{
				map[string]any{"priority": 0, "source": layer},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	path := filepath.Join(t.TempDir(), "deploy.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}
	return path
}

// runDeploymentDocument loads, deploys and drives one turn, returning the text
// the graph produced.
func runDeploymentDocument(t *testing.T, kinds deploy.Kinds, document string) string {
	t.Helper()
	specs, err := deploy.Loader{Kinds: kinds}.LoadFile(context.Background(), document)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	manager := newManager(t, kinds, testHost(t))
	if _, err := manager.Deploy(context.Background(), specs...); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	return turn(t, manager, "g")
}

// runtimeLayerBody is the substrate a document has to point at: a local boundary
// and one writable root, rooted at the test's temporary directory.
func runtimeLayerBody(t *testing.T) string {
	t.Helper()
	root := filepath.ToSlash(t.TempDir())
	return "version: 1\n" +
		"name: doc-layer\n" +
		"sandbox: {backend: local, writes: [\"" + root + "\"]}\n" +
		"files: {roots: [\"" + root + "\"]}\n"
}

// writeRuntimeLayer is runtimeLayerBody on disk.
func writeRuntimeLayer(t *testing.T) string {
	t.Helper()
	return writeTempYAML(t, "runtime.yaml", runtimeLayerBody(t))
}

// A malformed document fails at load, before any resource is built.
func TestGraphEngineValidatesTheDocumentAtLoad(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(enginesgraph.Engine(nodes.WithKinds(fixedKind{text: "x"})))
	path := writeGraphDocument(t, `version: 1
entry: a
nodes:
  - {id: a, kind: fixed}
bogus: true
`)
	_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(), []byte(
		"version: 1\nagents:\n  - name: g\n    engine:\n      kind: graph\n"+
			"      settings: {source: {kind: file, settings: {path: "+path+"}}}\n"))
	if err == nil {
		t.Fatal("a document with an unknown field was loaded")
	}
}
