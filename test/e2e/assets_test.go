package e2e_test

import (
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/agent/graph/nodes"
	"github.com/GizClaw/flowcraft/core/config"
)

// TestTheShippedGraphAssetsCompile is the cheapest guard a documentation fixture
// can have: the graph documents that ship as templates are compiled by the engine
// that will run them.
//
// It exists because they had drifted. The minimal-graph template named `END` while
// the engine's terminal node is `end`, and its edges tested `len(pending)` while a
// condition sees the state as `state.pending` — the first is a load error the day
// somebody copies the template, the second is worse, because a map environment
// reads an unknown name as nil, so `len(pending) > 0` compiles, never fires, and
// the run quietly takes the other branch. Neither was caught by the deploy
// validator: that one checks the shape of a deployment document, not that a graph
// can be built.
func TestTheShippedGraphAssetsCompile(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "skills", "flowcraft-deploy", "assets", "*", "graph.yaml"))
	if err != nil {
		t.Fatalf("glob the shipped assets: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("found no shipped graph assets; the glob is wrong and this test proves nothing")
	}
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			t.Fatalf("resolve %s: %v", path, err)
		}
		if _, err := nodes.Open(t.Context(), absolute, config.File(absolute)); err != nil {
			t.Errorf("%s does not compile: %v", path, err)
		}
	}
}
