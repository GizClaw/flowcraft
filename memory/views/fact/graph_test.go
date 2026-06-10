package fact

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestGraphDescriptorDefaultsAndOptions(t *testing.T) {
	graph := NewGraph(nil)

	got := graph.Descriptor()
	if got.ID != DefaultGraphID {
		t.Fatalf("Descriptor ID = %q, want %q", got.ID, DefaultGraphID)
	}
	if got.Kind != views.KindFactGraph {
		t.Fatalf("Descriptor Kind = %q, want %q", got.Kind, views.KindFactGraph)
	}
	if got.Version != DefaultGraphVersion {
		t.Fatalf("Descriptor Version = %q, want %q", got.Version, DefaultGraphVersion)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Descriptor Validate() error = %v", err)
	}

	graph = NewGraph(nil, WithGraphID("project-graph"), WithGraphVersion("v-test"))
	got = graph.Descriptor()
	want := views.Descriptor{
		ID:      "project-graph",
		Kind:    views.KindFactGraph,
		Version: "v-test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Descriptor = %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("custom Descriptor Validate() error = %v", err)
	}
}

func TestGraphNilStoreReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	graph := NewGraph(nil)

	if _, err := graph.PutNode(ctx, validNode("node-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutNode nil store error = %v, want validation", err)
	}
	if _, _, err := graph.GetNode(ctx, "node-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetNode nil store error = %v, want validation", err)
	}
	if _, err := graph.ListNodes(ctx, NodeListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListNodes nil store error = %v, want validation", err)
	}
	if err := graph.DeleteNode(ctx, "node-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteNode nil store error = %v, want validation", err)
	}
	if _, err := graph.PutEdge(ctx, validEdge("edge-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutEdge nil store error = %v, want validation", err)
	}
	if _, _, err := graph.GetEdge(ctx, "edge-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetEdge nil store error = %v, want validation", err)
	}
	if _, err := graph.ListEdges(ctx, EdgeListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListEdges nil store error = %v, want validation", err)
	}
	if err := graph.DeleteEdge(ctx, "edge-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEdge nil store error = %v, want validation", err)
	}
}

func TestGraphPutNodeValidation(t *testing.T) {
	ctx := context.Background()
	graph := NewGraph(&fakeGraphStore{})

	tests := []struct {
		name   string
		mutate func(*Node)
	}{
		{
			name: "missing id",
			mutate: func(node *Node) {
				node.ID = ""
			},
		},
		{
			name: "missing label",
			mutate: func(node *Node) {
				node.Label = ""
			},
		},
		{
			name: "missing fact refs",
			mutate: func(node *Node) {
				node.FactRefs = nil
			},
		},
		{
			name: "missing fact id",
			mutate: func(node *Node) {
				node.FactRefs[0].FactID = ""
			},
		},
		{
			name: "missing signature",
			mutate: func(node *Node) {
				node.Signature = views.ViewSignature{}
			},
		},
		{
			name: "missing upstream refs",
			mutate: func(node *Node) {
				node.Signature.UpstreamViewRefs = nil
			},
		},
		{
			name: "upstream ref missing view id",
			mutate: func(node *Node) {
				node.Signature.UpstreamViewRefs[0].ViewID = ""
			},
		},
		{
			name: "invalid source ref",
			mutate: func(node *Node) {
				node.SourceRefs[0].Message.MessageID = ""
			},
		},
		{
			name: "invalid node kind",
			mutate: func(node *Node) {
				node.Kind = NodeKind("unknown")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := validNode("node-1")
			tt.mutate(&node)

			if _, err := graph.PutNode(ctx, node); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("PutNode(%s) error = %v, want validation", tt.name, err)
			}
		})
	}
}

func TestGraphPutEdgeValidation(t *testing.T) {
	ctx := context.Background()
	graph := NewGraph(&fakeGraphStore{})

	tests := []struct {
		name   string
		mutate func(*Edge)
	}{
		{
			name: "missing id",
			mutate: func(edge *Edge) {
				edge.ID = ""
			},
		},
		{
			name: "missing from",
			mutate: func(edge *Edge) {
				edge.From = ""
			},
		},
		{
			name: "missing to",
			mutate: func(edge *Edge) {
				edge.To = ""
			},
		},
		{
			name: "missing predicate",
			mutate: func(edge *Edge) {
				edge.Predicate = ""
			},
		},
		{
			name: "missing fact refs",
			mutate: func(edge *Edge) {
				edge.FactRefs = nil
			},
		},
		{
			name: "missing fact id",
			mutate: func(edge *Edge) {
				edge.FactRefs[0].FactID = ""
			},
		},
		{
			name: "missing signature",
			mutate: func(edge *Edge) {
				edge.Signature = views.ViewSignature{}
			},
		},
		{
			name: "missing upstream refs",
			mutate: func(edge *Edge) {
				edge.Signature.UpstreamViewRefs = nil
			},
		},
		{
			name: "upstream ref missing view id",
			mutate: func(edge *Edge) {
				edge.Signature.UpstreamViewRefs[0].ViewID = ""
			},
		},
		{
			name: "invalid negative confidence",
			mutate: func(edge *Edge) {
				edge.Confidence = -0.1
			},
		},
		{
			name: "invalid high confidence",
			mutate: func(edge *Edge) {
				edge.Confidence = 1.1
			},
		},
		{
			name: "invalid validity range",
			mutate: func(edge *Edge) {
				from := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
				until := from.Add(-time.Minute)
				edge.ValidFrom = &from
				edge.ValidUntil = &until
			},
		},
		{
			name: "invalid source ref",
			mutate: func(edge *Edge) {
				edge.SourceRefs[0].Message.MessageID = ""
			},
		},
		{
			name: "invalid status",
			mutate: func(edge *Edge) {
				edge.Status = FactStatus("unknown")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edge := validEdge("edge-1")
			tt.mutate(&edge)

			if _, err := graph.PutEdge(ctx, edge); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("PutEdge(%s) error = %v, want validation", tt.name, err)
			}
		})
	}
}

func TestGraphGetListDeleteValidation(t *testing.T) {
	ctx := context.Background()
	graph := NewGraph(&fakeGraphStore{})
	invalidKind := NodeKind("unknown")
	invalidStatus := FactStatus("unknown")

	if _, _, err := graph.GetNode(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetNode empty id error = %v, want validation", err)
	}
	if err := graph.DeleteNode(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteNode empty id error = %v, want validation", err)
	}
	if _, err := graph.ListNodes(ctx, NodeListOptions{Kind: &invalidKind}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListNodes invalid kind error = %v, want validation", err)
	}
	if _, _, err := graph.GetEdge(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetEdge empty id error = %v, want validation", err)
	}
	if err := graph.DeleteEdge(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEdge empty id error = %v, want validation", err)
	}
	if _, err := graph.ListEdges(ctx, EdgeListOptions{Status: &invalidStatus}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListEdges invalid status error = %v, want validation", err)
	}
}

func TestGraphDelegatesNormalizesAndClonesBoundaries(t *testing.T) {
	ctx := context.Background()
	entity := NodeEntity
	active := FactActive
	store := &fakeGraphStore{
		putNodeOut:  validNode("node-put-out"),
		getNodeOut:  validNode("node-get-out"),
		getNodeOK:   true,
		listNodeOut: []Node{validNode("node-list-out")},
		putEdgeOut:  validEdge("edge-put-out"),
		getEdgeOut:  validEdge("edge-get-out"),
		getEdgeOK:   true,
		listEdgeOut: []Edge{validEdge("edge-list-out")},
	}
	graph := NewGraph(store)

	nodeInput := validNode("node-put-in")
	nodeInput.Kind = ""
	nodePut, err := graph.PutNode(ctx, nodeInput)
	if err != nil {
		t.Fatal(err)
	}
	if store.putNodeIn.ID != nodeInput.ID || store.putNodeIn.Kind != NodeEntity {
		t.Fatalf("store PutNode received %+v, want normalized entity node", store.putNodeIn)
	}
	if nodeInput.Kind != "" {
		t.Fatalf("PutNode mutated caller kind = %q, want empty", nodeInput.Kind)
	}
	assertNodeMutableState(t, nodeInput, "alias-1", "fact-1", "message-1", "fact-output:v1", "graph:v1", "v", "v", "PutNode shared mutable state with caller")

	nodePut.Aliases[0] = "mutated-return"
	nodePut.FactRefs[0].FactID = "mutated-return"
	nodePut.SourceRefs[0].Message.MessageID = "mutated-return"
	nodePut.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-return"
	nodePut.Signature.DiagnosticSignatures["projector"] = "mutated-return"
	nodePut.Metadata["k"] = "mutated-return"
	setNestedMetadata(nodePut.Metadata, "mutated-return")
	assertNodeMutableState(t, store.putNodeOut, "alias-1", "fact-1", "message-1", "fact-output:v1", "graph:v1", "v", "v", "PutNode return shared mutable state with store output")

	gotNode, ok, err := graph.GetNode(ctx, "node-get-out")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetNode ok = false, want true")
	}
	if store.getNodeID != "node-get-out" {
		t.Fatalf("store GetNode id = %q, want node-get-out", store.getNodeID)
	}
	gotNode.Aliases[0] = "mutated-get"
	gotNode.Metadata["k"] = "mutated-get"
	setNestedMetadata(gotNode.Metadata, "mutated-get")
	assertNodeMutableState(t, store.getNodeOut, "alias-1", "fact-1", "message-1", "fact-output:v1", "graph:v1", "v", "v", "GetNode result shared mutable state with store output")

	listedNodes, err := graph.ListNodes(ctx, NodeListOptions{
		AfterID: "node-a",
		Limit:   2,
		Kind:    &entity,
		Label:   "Coffee",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.listNodeOpts.AfterID != "node-a" || store.listNodeOpts.Limit != 2 || store.listNodeOpts.Label != "Coffee" {
		t.Fatalf("store ListNodes options = %+v, want delegated options", store.listNodeOpts)
	}
	if store.listNodeOpts.Kind == nil || *store.listNodeOpts.Kind != NodeEntity {
		t.Fatalf("store ListNodes kind = %+v, want entity", store.listNodeOpts.Kind)
	}
	if entity != NodeEntity {
		t.Fatalf("ListNodes shared Kind option with caller; kind = %q", entity)
	}
	listedNodes[0].Aliases[0] = "mutated-list"
	listedNodes[0].Metadata["k"] = "mutated-list"
	setNestedMetadata(listedNodes[0].Metadata, "mutated-list")
	assertNodeMutableState(t, store.listNodeOut[0], "alias-1", "fact-1", "message-1", "fact-output:v1", "graph:v1", "v", "v", "ListNodes result shared mutable state with store output")

	edgeInput := validEdge("edge-put-in")
	edgeInput.Status = ""
	edgePut, err := graph.PutEdge(ctx, edgeInput)
	if err != nil {
		t.Fatal(err)
	}
	if store.putEdgeIn.ID != edgeInput.ID || store.putEdgeIn.Status != FactActive {
		t.Fatalf("store PutEdge received %+v, want normalized active edge", store.putEdgeIn)
	}
	if edgeInput.Status != "" {
		t.Fatalf("PutEdge mutated caller status = %q, want empty", edgeInput.Status)
	}
	assertEdgeMutableState(t, edgeInput, "fact-1", "message-1", "fact-output:v1", "graph:v1", "v", "v", "PutEdge shared mutable state with caller")

	edgePut.FactRefs[0].FactID = "mutated-return"
	edgePut.SourceRefs[0].Message.MessageID = "mutated-return"
	edgePut.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-return"
	edgePut.Signature.DiagnosticSignatures["projector"] = "mutated-return"
	edgePut.Metadata["k"] = "mutated-return"
	setNestedMetadata(edgePut.Metadata, "mutated-return")
	*edgePut.ValidFrom = edgePut.ValidFrom.Add(time.Hour)
	assertEdgeMutableState(t, store.putEdgeOut, "fact-1", "message-1", "fact-output:v1", "graph:v1", "v", "v", "PutEdge return shared mutable state with store output")

	gotEdge, ok, err := graph.GetEdge(ctx, "edge-get-out")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetEdge ok = false, want true")
	}
	if store.getEdgeID != "edge-get-out" {
		t.Fatalf("store GetEdge id = %q, want edge-get-out", store.getEdgeID)
	}
	gotEdge.Metadata["k"] = "mutated-get"
	setNestedMetadata(gotEdge.Metadata, "mutated-get")
	assertEdgeMutableState(t, store.getEdgeOut, "fact-1", "message-1", "fact-output:v1", "graph:v1", "v", "v", "GetEdge result shared mutable state with store output")

	listedEdges, err := graph.ListEdges(ctx, EdgeListOptions{
		AfterID:   "edge-a",
		Limit:     2,
		From:      "user:123",
		To:        "value:coffee",
		Predicate: "likes",
		Status:    &active,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.listEdgeOpts.AfterID != "edge-a" || store.listEdgeOpts.Limit != 2 || store.listEdgeOpts.From != "user:123" || store.listEdgeOpts.To != "value:coffee" || store.listEdgeOpts.Predicate != "likes" {
		t.Fatalf("store ListEdges options = %+v, want delegated options", store.listEdgeOpts)
	}
	if store.listEdgeOpts.Status == nil || *store.listEdgeOpts.Status != FactActive {
		t.Fatalf("store ListEdges status = %+v, want active", store.listEdgeOpts.Status)
	}
	if active != FactActive {
		t.Fatalf("ListEdges shared Status option with caller; status = %q", active)
	}
	listedEdges[0].Metadata["k"] = "mutated-list"
	setNestedMetadata(listedEdges[0].Metadata, "mutated-list")
	assertEdgeMutableState(t, store.listEdgeOut[0], "fact-1", "message-1", "fact-output:v1", "graph:v1", "v", "v", "ListEdges result shared mutable state with store output")

	if err := graph.DeleteNode(ctx, "node-delete"); err != nil {
		t.Fatal(err)
	}
	if store.deleteNodeID != "node-delete" {
		t.Fatalf("store DeleteNode id = %q, want node-delete", store.deleteNodeID)
	}
	if err := graph.DeleteEdge(ctx, "edge-delete"); err != nil {
		t.Fatal(err)
	}
	if store.deleteEdgeID != "edge-delete" {
		t.Fatalf("store DeleteEdge id = %q, want edge-delete", store.deleteEdgeID)
	}
}

func validNode(id NodeID) Node {
	created := time.Date(2026, 6, 9, 1, 2, 3, 0, time.UTC)
	updated := time.Date(2026, 6, 9, 4, 5, 6, 0, time.UTC)
	return Node{
		ID:      id,
		Kind:    NodeEntity,
		Label:   "Coffee",
		Aliases: []string{"alias-1"},
		FactRefs: []FactRef{{
			FactID: "fact-1",
			Role:   "subject",
		}},
		SourceRefs: []views.SourceRef{validGraphSourceRef()},
		Signature:  validGraphSignature(),
		CreatedAt:  created,
		UpdatedAt:  updated,
		Metadata:   validGraphMetadata(),
	}
}

func validEdge(id EdgeID) Edge {
	created := time.Date(2026, 6, 9, 1, 2, 3, 0, time.UTC)
	updated := time.Date(2026, 6, 9, 4, 5, 6, 0, time.UTC)
	validFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	validUntil := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	return Edge{
		ID:         id,
		From:       "user:123",
		To:         "value:coffee",
		Predicate:  "likes",
		Status:     FactActive,
		Confidence: 0.8,
		ValidFrom:  &validFrom,
		ValidUntil: &validUntil,
		FactRefs: []FactRef{{
			FactID: "fact-1",
			Role:   "edge",
		}},
		SourceRefs: []views.SourceRef{validGraphSourceRef()},
		Signature:  validGraphSignature(),
		CreatedAt:  created,
		UpdatedAt:  updated,
		Metadata:   validGraphMetadata(),
	}
}

func validGraphSourceRef() views.SourceRef {
	return views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: "conversation-1",
			MessageID:      "message-1",
			Span:           &views.Span{Start: 0, End: 10},
		},
	}
}

func validGraphSignature() views.ViewSignature {
	return views.ViewSignature{
		ViewID: DefaultGraphID,
		UpstreamViewRefs: []views.UpstreamViewRef{{
			ViewID:          DefaultLedgerID,
			OutputSignature: "fact-output:v1",
			RecordKey:       "fact-1",
		}},
		DiagnosticSignatures: map[string]string{"projector": "graph:v1"},
	}
}

func validGraphMetadata() map[string]any {
	return map[string]any{
		"k": "v",
		"nested": map[string]any{
			"tag":   "v",
			"items": []any{"v", map[string]any{"inner": "v"}},
		},
	}
}

func assertNodeMutableState(t *testing.T, node Node, alias string, factID FactID, messageID, upstreamOutput, diagnostic, metadata, nestedMetadata, message string) {
	t.Helper()

	if node.Aliases[0] != alias {
		t.Fatalf("%s; alias = %q, want %q", message, node.Aliases[0], alias)
	}
	if node.FactRefs[0].FactID != factID {
		t.Fatalf("%s; fact ref = %q, want %q", message, node.FactRefs[0].FactID, factID)
	}
	if node.SourceRefs[0].Message.MessageID != messageID {
		t.Fatalf("%s; source ref message id = %q, want %q", message, node.SourceRefs[0].Message.MessageID, messageID)
	}
	if node.Signature.UpstreamViewRefs[0].OutputSignature != upstreamOutput {
		t.Fatalf("%s; upstream output = %q, want %q", message, node.Signature.UpstreamViewRefs[0].OutputSignature, upstreamOutput)
	}
	if node.Signature.DiagnosticSignatures["projector"] != diagnostic {
		t.Fatalf("%s; diagnostic signature = %q, want %q", message, node.Signature.DiagnosticSignatures["projector"], diagnostic)
	}
	if node.Metadata["k"] != metadata {
		t.Fatalf("%s; metadata = %#v, want %q", message, node.Metadata["k"], metadata)
	}
	assertNestedMetadata(t, node.Metadata, nestedMetadata, message)
}

func assertEdgeMutableState(t *testing.T, edge Edge, factID FactID, messageID, upstreamOutput, diagnostic, metadata, nestedMetadata, message string) {
	t.Helper()

	if edge.FactRefs[0].FactID != factID {
		t.Fatalf("%s; fact ref = %q, want %q", message, edge.FactRefs[0].FactID, factID)
	}
	if edge.SourceRefs[0].Message.MessageID != messageID {
		t.Fatalf("%s; source ref message id = %q, want %q", message, edge.SourceRefs[0].Message.MessageID, messageID)
	}
	if edge.Signature.UpstreamViewRefs[0].OutputSignature != upstreamOutput {
		t.Fatalf("%s; upstream output = %q, want %q", message, edge.Signature.UpstreamViewRefs[0].OutputSignature, upstreamOutput)
	}
	if edge.Signature.DiagnosticSignatures["projector"] != diagnostic {
		t.Fatalf("%s; diagnostic signature = %q, want %q", message, edge.Signature.DiagnosticSignatures["projector"], diagnostic)
	}
	if edge.Metadata["k"] != metadata {
		t.Fatalf("%s; metadata = %#v, want %q", message, edge.Metadata["k"], metadata)
	}
	assertNestedMetadata(t, edge.Metadata, nestedMetadata, message)
}

type fakeGraphStore struct {
	putNodeIn    Node
	putNodeOut   Node
	getNodeID    NodeID
	getNodeOut   Node
	getNodeOK    bool
	listNodeOpts NodeListOptions
	listNodeOut  []Node
	deleteNodeID NodeID
	putEdgeIn    Edge
	putEdgeOut   Edge
	getEdgeID    EdgeID
	getEdgeOut   Edge
	getEdgeOK    bool
	listEdgeOpts EdgeListOptions
	listEdgeOut  []Edge
	deleteEdgeID EdgeID
}

func (s *fakeGraphStore) PutNode(_ context.Context, node Node) (Node, error) {
	s.putNodeIn = node
	node.Aliases[0] = "mutated-store-input"
	node.FactRefs[0].FactID = "mutated-store-input"
	node.SourceRefs[0].Message.MessageID = "mutated-store-input"
	node.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-store-input"
	node.Signature.DiagnosticSignatures["projector"] = "mutated-store-input"
	node.Metadata["k"] = "mutated-store-input"
	setNestedMetadata(node.Metadata, "mutated-store-input")
	return s.putNodeOut, nil
}

func (s *fakeGraphStore) GetNode(_ context.Context, id NodeID) (Node, bool, error) {
	s.getNodeID = id
	return s.getNodeOut, s.getNodeOK, nil
}

func (s *fakeGraphStore) ListNodes(_ context.Context, opts NodeListOptions) ([]Node, error) {
	s.listNodeOpts = cloneNodeListOptions(opts)
	if opts.Kind != nil {
		*opts.Kind = NodeValue
	}
	return s.listNodeOut, nil
}

func (s *fakeGraphStore) DeleteNode(_ context.Context, id NodeID) error {
	s.deleteNodeID = id
	return nil
}

func (s *fakeGraphStore) PutEdge(_ context.Context, edge Edge) (Edge, error) {
	s.putEdgeIn = edge
	edge.FactRefs[0].FactID = "mutated-store-input"
	edge.SourceRefs[0].Message.MessageID = "mutated-store-input"
	edge.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-store-input"
	edge.Signature.DiagnosticSignatures["projector"] = "mutated-store-input"
	edge.Metadata["k"] = "mutated-store-input"
	setNestedMetadata(edge.Metadata, "mutated-store-input")
	*edge.ValidFrom = edge.ValidFrom.Add(time.Hour)
	return s.putEdgeOut, nil
}

func (s *fakeGraphStore) GetEdge(_ context.Context, id EdgeID) (Edge, bool, error) {
	s.getEdgeID = id
	return s.getEdgeOut, s.getEdgeOK, nil
}

func (s *fakeGraphStore) ListEdges(_ context.Context, opts EdgeListOptions) ([]Edge, error) {
	s.listEdgeOpts = cloneEdgeListOptions(opts)
	if opts.Status != nil {
		*opts.Status = FactRetracted
	}
	return s.listEdgeOut, nil
}

func (s *fakeGraphStore) DeleteEdge(_ context.Context, id EdgeID) error {
	s.deleteEdgeID = id
	return nil
}
