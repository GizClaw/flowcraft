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
	"github.com/GizClaw/flowcraft/core/message"
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
	kinds := deploy.BuiltinKinds().WithEngines(deploy.GraphEngine(nodes.WithKinds(fixedKind{text: "hello"})))
	manager := newManager(t, kinds, testHost(t))
	spec := testSpec(t, "g", "graph")
	spec.Engine.Settings = rawSettings(map[string]any{
		"path": writeGraphDocument(t, fixedGraph),
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

// A malformed document fails at load, before any resource is built.
func TestGraphEngineValidatesTheDocumentAtLoad(t *testing.T) {
	kinds := deploy.BuiltinKinds().WithEngines(deploy.GraphEngine(nodes.WithKinds(fixedKind{text: "x"})))
	path := writeGraphDocument(t, `version: 1
entry: a
nodes:
  - {id: a, kind: fixed}
bogus: true
`)
	_, err := deploy.Loader{Kinds: kinds}.Load(context.Background(), []byte(
		"version: 1\nagents:\n  - name: g\n    engine:\n      kind: graph\n"+
			"      settings: {path: "+path+"}\n"))
	if err == nil {
		t.Fatal("a document with an unknown field was loaded")
	}
}
