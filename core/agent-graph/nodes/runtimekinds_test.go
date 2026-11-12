package nodes_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// probeKind exists to prove the hook registered it: the test graph below names a
// kind nothing else supplies.
type probeKind struct{ seen *runtime.Runtime }

func (*probeKind) Name() string { return "probe" }

func (*probeKind) Invoke(context.Context, graph.Invoke) (graph.NodeStream, error) {
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

// TestRuntimeKindsAreBuiltFromTheRuntime pins the hook a kind needs when it
// dispatches through the substrate: the host cannot build such a kind, because its
// table exists before any runtime does.
func TestRuntimeKindsAreBuiltFromTheRuntime(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	var seen *runtime.Runtime
	kind := &probeKind{}
	graph, err := nodes.Open(context.Background(), rt, writeProbeDocument(t),
		nodes.WithRuntimeKinds(func(got *runtime.Runtime) ([]graph.NodeKind, error) {
			seen = got
			kind.seen = got
			return []graph.NodeKind{kind}, nil
		}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if seen != rt {
		t.Fatalf("the hook saw %v, want the runtime the graph was opened for", seen)
	}
	if graph == nil || kind.seen != rt {
		t.Fatal("the kind was not registered")
	}
}

func TestRuntimeKindsReportAFailure(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	_, err := nodes.Open(context.Background(), rt, writeProbeDocument(t),
		nodes.WithRuntimeKinds(func(*runtime.Runtime) ([]graph.NodeKind, error) {
			return nil, errors.New("a kind that cannot be built")
		}))
	if err == nil {
		t.Fatal("a failing hook must fail the build")
	}
}
