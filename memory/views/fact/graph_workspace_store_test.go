package fact

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestGraphWorkspaceStoreNilWorkspaceReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	store := NewGraphWorkspaceStore(nil)

	if _, err := store.PutNode(ctx, validNode("node-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutNode nil workspace error = %v, want validation", err)
	}
	if _, _, err := store.GetNode(ctx, "node-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetNode nil workspace error = %v, want validation", err)
	}
	if _, err := store.ListNodes(ctx, NodeListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListNodes nil workspace error = %v, want validation", err)
	}
	if err := store.DeleteNode(ctx, "node-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteNode nil workspace error = %v, want validation", err)
	}
	if _, err := store.PutEdge(ctx, validEdge("edge-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutEdge nil workspace error = %v, want validation", err)
	}
	if _, _, err := store.GetEdge(ctx, "edge-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetEdge nil workspace error = %v, want validation", err)
	}
	if _, err := store.ListEdges(ctx, EdgeListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListEdges nil workspace error = %v, want validation", err)
	}
	if err := store.DeleteEdge(ctx, "edge-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEdge nil workspace error = %v, want validation", err)
	}
}

func TestGraphWorkspaceStorePutGetDeepClone(t *testing.T) {
	ctx := context.Background()
	store := NewGraphWorkspaceStore(workspace.NewMemWorkspace())

	node := validNode("node-1")
	node.Kind = ""
	nodePut, err := store.PutNode(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	if nodePut.Kind != NodeEntity {
		t.Fatalf("PutNode kind = %q, want entity", nodePut.Kind)
	}
	node.Aliases[0] = "mutated-input"
	node.FactRefs[0].FactID = "mutated-input"
	node.SourceRefs[0].Message.MessageID = "mutated-input"
	node.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-input"
	node.Signature.DiagnosticSignatures["projector"] = "mutated-input"
	node.Metadata["k"] = "mutated-input"
	setNestedMetadata(node.Metadata, "mutated-input")
	nodePut.Aliases[0] = "mutated-put"
	nodePut.Metadata["k"] = "mutated-put"
	setNestedMetadata(nodePut.Metadata, "mutated-put")

	wantNode := validNode("node-1")
	gotNode, ok, err := store.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetNode ok = false, want true")
	}
	assertGraphNodeEqual(t, gotNode, wantNode)

	gotNode.Aliases[0] = "mutated-get"
	gotNode.Metadata["k"] = "mutated-get"
	setNestedMetadata(gotNode.Metadata, "mutated-get")
	nodeAgain, ok, err := store.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetNode after mutation ok = false, want true")
	}
	assertGraphNodeEqual(t, nodeAgain, wantNode)

	edge := validEdge("edge-1")
	edge.Status = ""
	edgePut, err := store.PutEdge(ctx, edge)
	if err != nil {
		t.Fatal(err)
	}
	if edgePut.Status != FactActive {
		t.Fatalf("PutEdge status = %q, want active", edgePut.Status)
	}
	edge.FactRefs[0].FactID = "mutated-input"
	edge.SourceRefs[0].Message.MessageID = "mutated-input"
	edge.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-input"
	edge.Signature.DiagnosticSignatures["projector"] = "mutated-input"
	edge.Metadata["k"] = "mutated-input"
	setNestedMetadata(edge.Metadata, "mutated-input")
	*edge.ValidFrom = edge.ValidFrom.AddDate(0, 0, 1)
	edgePut.FactRefs[0].FactID = "mutated-put"
	edgePut.Metadata["k"] = "mutated-put"
	setNestedMetadata(edgePut.Metadata, "mutated-put")
	*edgePut.ValidFrom = edgePut.ValidFrom.AddDate(0, 0, 1)

	wantEdge := validEdge("edge-1")
	gotEdge, ok, err := store.GetEdge(ctx, "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetEdge ok = false, want true")
	}
	assertGraphEdgeEqual(t, gotEdge, wantEdge)

	gotEdge.Metadata["k"] = "mutated-get"
	setNestedMetadata(gotEdge.Metadata, "mutated-get")
	*gotEdge.ValidFrom = gotEdge.ValidFrom.AddDate(0, 0, 1)
	edgeAgain, ok, err := store.GetEdge(ctx, "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetEdge after mutation ok = false, want true")
	}
	assertGraphEdgeEqual(t, edgeAgain, wantEdge)
}

func TestGraphWorkspaceStoreListOrderAfterIDLimitAndFilters(t *testing.T) {
	ctx := context.Background()
	store := NewGraphWorkspaceStore(workspace.NewMemWorkspace())

	nodes := map[NodeID]Node{
		"bravo":   validNode("bravo"),
		"alpha":   validNode("alpha"),
		"delta":   validNode("delta"),
		"charlie": validNode("charlie"),
	}
	bravoNode := nodes["bravo"]
	bravoNode.Kind = NodeValue
	bravoNode.Label = "Tea"
	nodes["bravo"] = bravoNode
	charlieNode := nodes["charlie"]
	charlieNode.Label = "Tea"
	nodes["charlie"] = charlieNode

	for _, id := range []NodeID{"bravo", "alpha", "delta", "charlie"} {
		if _, err := store.PutNode(ctx, nodes[id]); err != nil {
			t.Fatal(err)
		}
	}

	allNodes, err := store.ListNodes(ctx, NodeListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphNodeIDs(t, allNodes, []NodeID{"alpha", "bravo", "charlie", "delta"})

	nodeAfterLimited, err := store.ListNodes(ctx, NodeListOptions{AfterID: "alpha", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphNodeIDs(t, nodeAfterLimited, []NodeID{"bravo", "charlie"})

	valueKind := NodeValue
	valueNodes, err := store.ListNodes(ctx, NodeListOptions{Kind: &valueKind})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphNodeIDs(t, valueNodes, []NodeID{"bravo"})

	teaNodes, err := store.ListNodes(ctx, NodeListOptions{Label: "Tea"})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphNodeIDs(t, teaNodes, []NodeID{"bravo", "charlie"})

	entityKind := NodeEntity
	combinedNodes, err := store.ListNodes(ctx, NodeListOptions{Limit: 2, Kind: &entityKind, Label: "Coffee"})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphNodeIDs(t, combinedNodes, []NodeID{"alpha", "delta"})

	edges := map[EdgeID]Edge{
		"bravo":   validEdge("bravo"),
		"alpha":   validEdge("alpha"),
		"delta":   validEdge("delta"),
		"charlie": validEdge("charlie"),
	}
	bravoEdge := edges["bravo"]
	bravoEdge.From = "user:456"
	edges["bravo"] = bravoEdge
	charlieEdge := edges["charlie"]
	charlieEdge.To = "value:tea"
	charlieEdge.Predicate = "dislikes"
	edges["charlie"] = charlieEdge
	deltaEdge := edges["delta"]
	deltaEdge.Status = FactSuperseded
	edges["delta"] = deltaEdge

	for _, id := range []EdgeID{"bravo", "alpha", "delta", "charlie"} {
		if _, err := store.PutEdge(ctx, edges[id]); err != nil {
			t.Fatal(err)
		}
	}

	allEdges, err := store.ListEdges(ctx, EdgeListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphEdgeIDs(t, allEdges, []EdgeID{"alpha", "bravo", "charlie", "delta"})

	edgeAfterLimited, err := store.ListEdges(ctx, EdgeListOptions{AfterID: "alpha", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphEdgeIDs(t, edgeAfterLimited, []EdgeID{"bravo", "charlie"})

	fromFiltered, err := store.ListEdges(ctx, EdgeListOptions{From: "user:123"})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphEdgeIDs(t, fromFiltered, []EdgeID{"alpha", "charlie", "delta"})

	toFiltered, err := store.ListEdges(ctx, EdgeListOptions{To: "value:coffee"})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphEdgeIDs(t, toFiltered, []EdgeID{"alpha", "bravo", "delta"})

	predicateFiltered, err := store.ListEdges(ctx, EdgeListOptions{Predicate: "likes"})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphEdgeIDs(t, predicateFiltered, []EdgeID{"alpha", "bravo", "delta"})

	status := FactSuperseded
	statusFiltered, err := store.ListEdges(ctx, EdgeListOptions{Status: &status})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphEdgeIDs(t, statusFiltered, []EdgeID{"delta"})

	active := FactActive
	combinedEdges, err := store.ListEdges(ctx, EdgeListOptions{
		Limit:     2,
		From:      "user:123",
		To:        "value:coffee",
		Predicate: "likes",
		Status:    &active,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertGraphEdgeIDs(t, combinedEdges, []EdgeID{"alpha"})
}

func TestGraphWorkspaceStoreDeleteEdge(t *testing.T) {
	ctx := context.Background()
	store := NewGraphWorkspaceStore(workspace.NewMemWorkspace())

	if _, err := store.PutEdge(ctx, validEdge("edge-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutEdge(ctx, validEdge("edge-2")); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteEdge(ctx, "edge-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.GetEdge(ctx, "edge-1"); err != nil || ok {
		t.Fatalf("GetEdge deleted edge ok = %v err %v, want false nil", ok, err)
	}
	if err := store.DeleteEdge(ctx, "edge-1"); err != nil {
		t.Fatalf("second DeleteEdge error = %v, want nil", err)
	}
	if got, ok, err := store.GetEdge(ctx, "edge-2"); err != nil || !ok || got.ID != "edge-2" {
		t.Fatalf("GetEdge kept edge = %+v ok %v err %v, want edge-2 true nil", got, ok, err)
	}
}

func TestGraphWorkspaceStoreDeleteNodeCascadesEdges(t *testing.T) {
	ctx := context.Background()
	store := NewGraphWorkspaceStore(workspace.NewMemWorkspace())

	for _, id := range []NodeID{"node-a", "node-b", "node-c"} {
		if _, err := store.PutNode(ctx, validNode(id)); err != nil {
			t.Fatal(err)
		}
	}
	edgeAB := validEdge("edge-ab")
	edgeAB.From = "node-a"
	edgeAB.To = "node-b"
	edgeBC := validEdge("edge-bc")
	edgeBC.From = "node-b"
	edgeBC.To = "node-c"
	edgeCC := validEdge("edge-cc")
	edgeCC.From = "node-c"
	edgeCC.To = "node-c"
	for _, edge := range []Edge{edgeAB, edgeBC, edgeCC} {
		if _, err := store.PutEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteNode(ctx, "node-b"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.GetNode(ctx, "node-b"); err != nil || ok {
		t.Fatalf("GetNode deleted node ok = %v err %v, want false nil", ok, err)
	}
	if _, ok, err := store.GetEdge(ctx, "edge-ab"); err != nil || ok {
		t.Fatalf("GetEdge edge-ab ok = %v err %v, want false nil", ok, err)
	}
	if _, ok, err := store.GetEdge(ctx, "edge-bc"); err != nil || ok {
		t.Fatalf("GetEdge edge-bc ok = %v err %v, want false nil", ok, err)
	}
	if got, ok, err := store.GetEdge(ctx, "edge-cc"); err != nil || !ok || got.ID != "edge-cc" {
		t.Fatalf("GetEdge kept edge = %+v ok %v err %v, want edge-cc true nil", got, ok, err)
	}
	if err := store.DeleteNode(ctx, "node-b"); err != nil {
		t.Fatalf("second DeleteNode error = %v, want nil", err)
	}
}

func TestGraphWorkspaceStorePathSegmentPrefixDefaultCustomAndExplicitEmpty(t *testing.T) {
	ctx := context.Background()

	t.Run("default", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewGraphWorkspaceStore(ws)
		node := validNode("node/with/slash")
		edge := validEdge("edge/with/slash")
		if _, err := store.PutNode(ctx, node); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}

		nodeSegment := store.pathSegment(string(node.ID))
		edgeSegment := store.pathSegment(string(edge.ID))
		assertSafeGraphSegment(t, store, nodeSegment, string(node.ID), "fgraph_")
		assertSafeGraphSegment(t, store, edgeSegment, string(edge.ID), "fgraph_")
		assertGraphPathExists(t, ctx, ws, "graph/nodes/"+nodeSegment+".json")
		assertGraphPathExists(t, ctx, ws, "graph/edges/"+edgeSegment+".json")
		assertGraphPathMissing(t, ctx, ws, "graph/nodes/"+string(node.ID)+".json")
		assertGraphPathMissing(t, ctx, ws, "graph/edges/"+string(edge.ID)+".json")
	})

	t.Run("custom", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewGraphWorkspaceStore(ws, WithGraphPathSegmentPrefix("custom_"))
		node := validNode("../node.json")
		edge := validEdge("../edge.json")
		if _, err := store.PutNode(ctx, node); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}

		nodeSegment := store.pathSegment(string(node.ID))
		edgeSegment := store.pathSegment(string(edge.ID))
		assertSafeGraphSegment(t, store, nodeSegment, string(node.ID), "custom_")
		assertSafeGraphSegment(t, store, edgeSegment, string(edge.ID), "custom_")
		assertGraphPathExists(t, ctx, ws, "graph/nodes/"+nodeSegment+".json")
		assertGraphPathExists(t, ctx, ws, "graph/edges/"+edgeSegment+".json")
		assertGraphPathMissing(t, ctx, ws, "graph/nodes/"+string(node.ID)+".json")
		assertGraphPathMissing(t, ctx, ws, "graph/edges/"+string(edge.ID)+".json")
	})

	t.Run("explicit empty", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewGraphWorkspaceStore(ws, WithGraphPathSegmentPrefix(""))
		node := validNode("node/with/slash")
		edge := validEdge("edge/with/slash")
		if _, err := store.PutNode(ctx, node); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}

		nodeSegment := store.pathSegment(string(node.ID))
		edgeSegment := store.pathSegment(string(edge.ID))
		if strings.HasPrefix(nodeSegment, defaultGraphPathSegmentPrefix) || strings.HasPrefix(edgeSegment, defaultGraphPathSegmentPrefix) {
			t.Fatalf("explicit empty prefix segments = %q/%q, should not use default prefix", nodeSegment, edgeSegment)
		}
		assertSafeGraphSegment(t, store, nodeSegment, string(node.ID), "")
		assertSafeGraphSegment(t, store, edgeSegment, string(edge.ID), "")
		assertGraphPathExists(t, ctx, ws, "graph/nodes/"+nodeSegment+".json")
		assertGraphPathExists(t, ctx, ws, "graph/edges/"+edgeSegment+".json")
	})
}

func TestGraphWorkspaceStoreMetadataJSONRoundTripSemantics(t *testing.T) {
	ctx := context.Background()
	store := NewGraphWorkspaceStore(workspace.NewMemWorkspace())
	node := validNode("node-1")
	edge := validEdge("edge-1")
	metadata := map[string]any{
		"int":  7,
		"bool": true,
		"nested": map[string]any{
			"count": 2,
			"items": []any{3, map[string]any{"inner": 4}},
		},
	}
	node.Metadata = cloneAnyMap(metadata)
	edge.Metadata = cloneAnyMap(metadata)

	if _, err := store.PutNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}

	gotNode, ok, err := store.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetNode ok = false, want true")
	}
	assertJSONMetadataSemantics(t, gotNode.Metadata)

	gotEdge, ok, err := store.GetEdge(ctx, "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetEdge ok = false, want true")
	}
	assertJSONMetadataSemantics(t, gotEdge.Metadata)
}

func TestGraphWorkspaceStoreValidationErrors(t *testing.T) {
	ctx := context.Background()
	store := NewGraphWorkspaceStore(workspace.NewMemWorkspace())
	invalidKind := NodeKind("unknown")
	invalidStatus := FactStatus("unknown")

	invalidNode := validNode("node-1")
	invalidNode.Label = ""
	if _, err := store.PutNode(ctx, invalidNode); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutNode invalid node error = %v, want validation", err)
	}
	if _, err := store.PutNode(ctx, validNode("")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutNode empty id error = %v, want validation", err)
	}
	if _, _, err := store.GetNode(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetNode empty id error = %v, want validation", err)
	}
	if _, err := store.ListNodes(ctx, NodeListOptions{Kind: &invalidKind}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListNodes invalid kind error = %v, want validation", err)
	}
	if err := store.DeleteNode(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteNode empty id error = %v, want validation", err)
	}

	invalidEdge := validEdge("edge-1")
	invalidEdge.Predicate = ""
	if _, err := store.PutEdge(ctx, invalidEdge); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutEdge invalid edge error = %v, want validation", err)
	}
	if _, err := store.PutEdge(ctx, validEdge("")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutEdge empty id error = %v, want validation", err)
	}
	if _, _, err := store.GetEdge(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetEdge empty id error = %v, want validation", err)
	}
	if _, err := store.ListEdges(ctx, EdgeListOptions{Status: &invalidStatus}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListEdges invalid status error = %v, want validation", err)
	}
	if err := store.DeleteEdge(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEdge empty id error = %v, want validation", err)
	}
}

func assertGraphNodeEqual(t *testing.T, got, want Node) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("node mismatch:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func assertGraphEdgeEqual(t *testing.T, got, want Edge) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edge mismatch:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func assertGraphNodeIDs(t *testing.T, nodes []Node, want []NodeID) {
	t.Helper()
	got := make([]NodeID, 0, len(nodes))
	for _, node := range nodes {
		got = append(got, node.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("node IDs = %v, want %v", got, want)
	}
}

func assertGraphEdgeIDs(t *testing.T, edges []Edge, want []EdgeID) {
	t.Helper()
	got := make([]EdgeID, 0, len(edges))
	for _, edge := range edges {
		got = append(got, edge.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("edge IDs = %v, want %v", got, want)
	}
}

func assertSafeGraphSegment(t *testing.T, store *GraphWorkspaceStore, segment, raw, wantPrefix string) {
	t.Helper()
	if !strings.HasPrefix(segment, wantPrefix) {
		t.Fatalf("segment %q for raw %q missing %q prefix", segment, raw, wantPrefix)
	}
	if strings.Contains(segment, "/") || segment == "." || segment == ".." {
		t.Fatalf("segment %q for raw %q is not path safe", segment, raw)
	}
	decoded, err := store.rawPathSegment(segment)
	if err != nil {
		t.Fatalf("rawPathSegment(%q) error = %v", segment, err)
	}
	if decoded != raw {
		t.Fatalf("rawPathSegment(%q) = %q, want %q", segment, decoded, raw)
	}
}

func assertGraphPathExists(t *testing.T, ctx context.Context, ws workspace.Workspace, path string) {
	t.Helper()
	if exists, err := ws.Exists(ctx, path); err != nil || !exists {
		t.Fatalf("path %q exists = %v err %v, want true nil", path, exists, err)
	}
}

func assertGraphPathMissing(t *testing.T, ctx context.Context, ws workspace.Workspace, path string) {
	t.Helper()
	if exists, err := ws.Exists(ctx, path); err != nil || exists {
		t.Fatalf("path %q exists = %v err %v, want false nil", path, exists, err)
	}
}

func assertJSONMetadataSemantics(t *testing.T, metadata map[string]any) {
	t.Helper()

	if metadata["int"] != float64(7) {
		t.Fatalf("metadata int = %#v, want float64(7)", metadata["int"])
	}
	if metadata["bool"] != true {
		t.Fatalf("metadata bool = %#v, want true", metadata["bool"])
	}
	nested, ok := metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata nested type = %T, want map[string]any", metadata["nested"])
	}
	if nested["count"] != float64(2) {
		t.Fatalf("metadata nested count = %#v, want float64(2)", nested["count"])
	}
	items, ok := nested["items"].([]any)
	if !ok {
		t.Fatalf("metadata nested items type = %T, want []any", nested["items"])
	}
	if items[0] != float64(3) {
		t.Fatalf("metadata nested items[0] = %#v, want float64(3)", items[0])
	}
	inner, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("metadata nested items[1] type = %T, want map[string]any", items[1])
	}
	if inner["inner"] != float64(4) {
		t.Fatalf("metadata nested inner = %#v, want float64(4)", inner["inner"])
	}
}
