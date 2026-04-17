package version

import (
	"testing"

	"github.com/GizClaw/flowcraft/internal/model"
)

func TestComputeChecksum(t *testing.T) {
	def := &model.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}},
	}

	c1 := ComputeChecksum(def)
	c2 := ComputeChecksum(def)
	if c1 != c2 {
		t.Fatal("checksum should be deterministic")
	}
	if len(c1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(c1))
	}

	// Different definition → different checksum
	def2 := &model.GraphDefinition{
		Name:  "test-2",
		Entry: "start",
		Nodes: []model.NodeDefinition{{ID: "start", Type: "template"}},
	}
	c3 := ComputeChecksum(def2)
	if c1 == c3 {
		t.Fatal("different definitions should have different checksums")
	}
}

func TestComputeDiff_NoChanges(t *testing.T) {
	def := &model.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []model.NodeDefinition{{ID: "start", Type: "llm"}},
		Edges: []model.EdgeDefinition{{From: "start", To: "__end__"}},
	}

	diff := computeDiff(def, def, 1, 2)
	if len(diff.NodesAdded) != 0 || len(diff.NodesRemoved) != 0 || len(diff.NodesChanged) != 0 {
		t.Fatal("expected no node changes")
	}
	if len(diff.EdgesAdded) != 0 || len(diff.EdgesRemoved) != 0 {
		t.Fatal("expected no edge changes")
	}
}

func TestComputeDiff_NodeAddedRemoved(t *testing.T) {
	from := &model.GraphDefinition{
		Name:  "test",
		Entry: "a",
		Nodes: []model.NodeDefinition{{ID: "a", Type: "llm"}, {ID: "b", Type: "template"}},
	}
	to := &model.GraphDefinition{
		Name:  "test",
		Entry: "a",
		Nodes: []model.NodeDefinition{{ID: "a", Type: "llm"}, {ID: "c", Type: "router"}},
	}

	diff := computeDiff(from, to, 1, 2)
	if len(diff.NodesAdded) != 1 || diff.NodesAdded[0].ID != "c" {
		t.Fatalf("expected node c added, got %v", diff.NodesAdded)
	}
	if len(diff.NodesRemoved) != 1 || diff.NodesRemoved[0].ID != "b" {
		t.Fatalf("expected node b removed, got %v", diff.NodesRemoved)
	}
}

func TestComputeDiff_NodeChanged(t *testing.T) {
	from := &model.GraphDefinition{
		Name:  "test",
		Entry: "a",
		Nodes: []model.NodeDefinition{{ID: "a", Type: "llm", Config: map[string]any{"model": "gpt-4"}}},
	}
	to := &model.GraphDefinition{
		Name:  "test",
		Entry: "a",
		Nodes: []model.NodeDefinition{{ID: "a", Type: "llm", Config: map[string]any{"model": "claude-3"}}},
	}

	diff := computeDiff(from, to, 1, 2)
	if len(diff.NodesChanged) != 1 {
		t.Fatalf("expected 1 changed node, got %d", len(diff.NodesChanged))
	}
	if diff.NodesChanged[0].NodeID != "a" {
		t.Fatalf("expected node a changed, got %q", diff.NodesChanged[0].NodeID)
	}
}

func TestComputeDiff_EdgeAddedRemoved(t *testing.T) {
	from := &model.GraphDefinition{
		Name:  "test",
		Entry: "a",
		Edges: []model.EdgeDefinition{{From: "a", To: "b"}},
	}
	to := &model.GraphDefinition{
		Name:  "test",
		Entry: "a",
		Edges: []model.EdgeDefinition{{From: "a", To: "c"}},
	}

	diff := computeDiff(from, to, 1, 2)
	if len(diff.EdgesAdded) != 1 {
		t.Fatalf("expected 1 edge added, got %d", len(diff.EdgesAdded))
	}
	if len(diff.EdgesRemoved) != 1 {
		t.Fatalf("expected 1 edge removed, got %d", len(diff.EdgesRemoved))
	}
}
