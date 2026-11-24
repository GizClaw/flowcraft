package nodes_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/agent/graph/internal/graphtest"
	"github.com/GizClaw/flowcraft/agent/graph/nodes"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/message"
)

// probeKind is a host's own node kind: it exists to prove that a document can name a kind nothing else
// supplies, and that the kind receives the session's substrate with the invocation rather than at build.
type probeKind struct {
	seen graph.Invoke
}

func (*probeKind) Name() string { return "probe" }

func (p *probeKind) Invoke(_ context.Context, in graph.Invoke) (graph.NodeStream, error) {
	p.seen = in
	return graph.Static(graph.Result{}), nil
}

func writeProbeDocument(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.yaml")
	body := `version: 1
entry: probe
nodes:
  - {id: probe, kind: probe}
edges:
  - {from: probe, to: end}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write document: %v", err)
	}
	return path
}

// TestACustomKindIsRegisteredWithWithKinds pins the registration path: a kind the host writes is added
// to the registry by WithKinds, and it needs nothing from the loader — no runtime, no hook — because
// everything session-scoped arrives with the invocation.
func TestACustomKindIsRegisteredWithWithKinds(t *testing.T) {
	rt := testRuntime(t, &stubProvider{response: textResponse("ok")})
	kind := &probeKind{}
	path := writeProbeDocument(t)
	built, err := nodes.Open(context.Background(), path, config.File(path), nodes.WithKinds(kind))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	program := graph.NewProgram(built)
	drv, err := program.Open(agent.SessionInfo{SessionID: "c1"})
	if err != nil {
		t.Fatalf("Open session: %v", err)
	}
	defer func() { _ = drv.Close() }()

	binding, tools, models, _ := rt.Substrate()
	req := agent.Request{
		Task:    agent.Task{ID: "t1"},
		Input:   message.NewTextMessage(message.RoleUser, "hi"),
		Store:   agenttest.NewMemStore(),
		Binding: binding,
		Tools:   tools,
		Models:  models,
	}
	for _, err := range drv.Run(context.Background(), req) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	// The four things a kind reads all came from the turn, not from construction.
	if !kind.seen.Binding.Valid() {
		t.Error("the kind saw no binding")
	}
	if kind.seen.Tools == nil {
		t.Error("the kind saw no tool surface")
	}
	if kind.seen.Models == nil {
		t.Error("the kind saw no model access")
	}
	if kind.seen.Node != "probe" {
		t.Errorf("node id = %q, want the document's id", kind.seen.Node)
	}
}

// A test drives a kind directly; the fixture stamps the same substrate a run would, so a case cannot
// pass by holding authority the deployed kind would not have.
func TestTheFixtureStampsTheSameSubstrateARunDoes(t *testing.T) {
	rt := graphtest.New(t, graphtest.Config{})
	in := graph.Invoke{}
	rt.Stamp(&in)
	if !in.Binding.Valid() || in.Tools == nil || in.Models == nil {
		t.Fatalf("stamped invocation = %+v, want the fixture's substrate", in)
	}
}
