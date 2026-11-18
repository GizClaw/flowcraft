package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/builtin"
	enginesgraph "github.com/GizClaw/flowcraft/core/deploy/engines/graph"
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

func writeGraphDocument(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write graph: %v", err)
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
	spec.Engine.Settings = rawSettings(map[string]any{
		"source": map[string]any{
			"kind":     "file",
			"settings": map[string]any{"path": writeGraphDocument(t, fixedGraph)},
		},
	})
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, ok := manager.Session("g", "conv-1")
	if !ok {
		t.Fatal("no session for the deployed graph")
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
	if text != "hello" {
		t.Errorf("text = %q, want the graph's output", text)
	}
}

// The whole path in one run: a deployment document on disk names a file source for
// its runtime layer and another for the graph, the loader resolves both, and the
// manager runs a turn. This is the only test that crosses document → source →
// manager → graph, as opposed to proving the halves separately.
func TestDeploymentDocumentRunsEndToEnd(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(enginesgraph.Engine(nodes.WithKinds(fixedKind{text: "hello"})))
	layer := writeRuntimeLayer(t)
	graphDocument := writeGraphDocument(t, fixedGraph)
	document := filepath.Join(t.TempDir(), "deploy.yaml")
	// Paths are quoted by encoding/json: a Windows temporary directory can hold a
	// space, and a YAML double-quoted scalar would corrupt its backslashes.
	layerPath, err := json.Marshal(layer)
	if err != nil {
		t.Fatalf("marshal layer path: %v", err)
	}
	graphPath, err := json.Marshal(graphDocument)
	if err != nil {
		t.Fatalf("marshal graph path: %v", err)
	}
	body := "version: 1\n" +
		"agents:\n" +
		"  - name: g\n" +
		"    engine:\n" +
		"      kind: graph\n" +
		"      settings:\n" +
		"        source: {kind: file, settings: {path: " + string(graphPath) + "}}\n" +
		"    runtime:\n" +
		"      layers:\n" +
		"        - priority: 0\n" +
		"          source: {kind: file, settings: {path: " + string(layerPath) + "}}\n"
	if err := os.WriteFile(document, []byte(body), 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}
	specs, err := deploy.Loader{Kinds: kinds}.LoadFile(context.Background(), document)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	manager := newManager(t, kinds, testHost(t))
	if _, err := manager.Deploy(context.Background(), specs...); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	session, ok := manager.Session("g", "conv-1")
	if !ok {
		t.Fatal("no session for the deployed agent")
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
	if text != "hello" {
		t.Errorf("text = %q, want the graph's output", text)
	}
}

// writeRuntimeLayer is the substrate a document has to point at: a local boundary
// and one writable root, rooted at the test's temporary directory.
func writeRuntimeLayer(t *testing.T) string {
	t.Helper()
	root := filepath.ToSlash(t.TempDir())
	body := "version: 1\n" +
		"name: doc-layer\n" +
		"sandbox: {backend: local, writes: [\"" + root + "\"]}\n" +
		"files: {roots: [\"" + root + "\"]}\n"
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write runtime layer: %v", err)
	}
	return path
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
