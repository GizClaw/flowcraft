package recent

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestSummaryDAGDescriptor(t *testing.T) {
	dag := NewSummaryDAG(nil, WithID("conversation-summary"), WithVersion("v-test"))

	got := dag.Descriptor()
	if got.ID != views.ID("conversation-summary") {
		t.Fatalf("Descriptor ID = %q, want conversation-summary", got.ID)
	}
	if got.Kind != views.KindSummaryDAG {
		t.Fatalf("Descriptor Kind = %q, want summary_dag", got.Kind)
	}
	if got.Version != "v-test" {
		t.Fatalf("Descriptor Version = %q, want v-test", got.Version)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Descriptor Validate() error = %v", err)
	}
}

func TestSummaryDAGNilStoreReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	dag := NewSummaryDAG(nil)

	if _, err := dag.PutNode(ctx, validNode("conversation-1", "node-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutNode nil store error = %v, want validation", err)
	}
	if _, _, err := dag.GetNode(ctx, "conversation-1", "node-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("GetNode nil store error = %v, want validation", err)
	}
	if _, err := dag.ListNodes(ctx, "conversation-1", ListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("ListNodes nil store error = %v, want validation", err)
	}
	if err := dag.DeleteConversation(ctx, "conversation-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteConversation nil store error = %v, want validation", err)
	}
}

func TestSummaryWorkspaceStorePutGetListDeleteConversationRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewSummaryWorkspaceStore(workspace.NewMemWorkspace())
	node := validNode("conversation-1", "node-1")

	put, err := store.PutNode(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	if put.ID != node.ID || put.ConversationID != node.ConversationID || put.Summary != node.Summary {
		t.Fatalf("PutNode = %+v, want original identity and summary", put)
	}

	got, ok, err := store.GetNode(ctx, "conversation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetNode ok = false, want true")
	}
	assertNodeEqual(t, got, node)

	listed, err := store.ListNodes(ctx, "conversation-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, listed, []NodeID{"node-1"})

	dag := NewSummaryDAG(store)
	fromDAG, ok, err := dag.GetNode(ctx, "conversation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || fromDAG.ID != "node-1" {
		t.Fatalf("SummaryDAG GetNode = %+v ok %v, want node-1 true", fromDAG, ok)
	}

	if err := store.DeleteConversation(ctx, "conversation-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.GetNode(ctx, "conversation-1", "node-1"); err != nil || ok {
		t.Fatalf("GetNode after DeleteConversation = ok %v err %v, want false nil", ok, err)
	}
	listed, err = store.ListNodes(ctx, "conversation-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("ListNodes after DeleteConversation returned %d nodes, want 0", len(listed))
	}
}

func TestSummaryWorkspaceStorePutValidatesSourceRefs(t *testing.T) {
	ctx := context.Background()
	store := NewSummaryWorkspaceStore(workspace.NewMemWorkspace())
	node := validNode("conversation-1", "node-1")
	node.SourceRefs = []views.SourceRef{{
		Kind:    views.SourceMessage,
		Message: &views.MessageSourceRef{ConversationID: "conversation-1"},
	}}

	if _, err := store.PutNode(ctx, node); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutNode invalid source ref error = %v, want validation error", err)
	}
}

func TestSummaryWorkspaceStoreRejectsUpstreamViewLineage(t *testing.T) {
	ctx := context.Background()
	store := NewSummaryWorkspaceStore(workspace.NewMemWorkspace())
	node := validNode("conversation-1", "node-1")
	node.Signature.UpstreamViewRefs = []views.UpstreamViewRef{{
		ViewID:          views.ID("recent-window"),
		OutputSignature: "window:v1",
		RecordKey:       "conversation-1",
	}}

	if _, err := store.PutNode(ctx, node); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("PutNode upstream view lineage error = %v, want validation error", err)
	}
}

func TestSummaryWorkspaceStorePutValidation(t *testing.T) {
	ctx := context.Background()
	store := NewSummaryWorkspaceStore(workspace.NewMemWorkspace())

	tests := []SummaryNode{
		{ConversationID: "conversation-1", Summary: "summary"},
		{ID: "node-1", Summary: "summary"},
		{ID: "node-1", ConversationID: "conversation-1"},
		{ID: "node-1", ConversationID: "conversation-1", Summary: "summary", Level: -1},
		{
			ID:             "node-1",
			ConversationID: "conversation-1",
			Summary:        "summary",
			Signature: views.ViewSignature{SourceRevisions: []views.SourceRevision{
				{Kind: views.SourceMessage, SourceKey: "msg-1", Revision: "1"},
				{Kind: views.SourceMessage, SourceKey: "msg-1", Revision: "2"},
			}},
		},
	}

	for _, node := range tests {
		if _, err := store.PutNode(ctx, node); err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("PutNode(%+v) error = %v, want validation error", node, err)
		}
	}
}

func TestSummaryWorkspaceStoreReturnsClonesWithoutSortingParents(t *testing.T) {
	ctx := context.Background()
	store := NewSummaryWorkspaceStore(workspace.NewMemWorkspace())
	node := validNode("conversation-1", "node-1")
	node.ParentIDs = []NodeID{"parent-b", "parent-a"}

	put, err := store.PutNode(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	put.ParentIDs[0] = "mutated-parent"
	put.SourceRefs[0].Message.MessageID = "mutated-message"
	put.Signature.SourceRevisions[0].Revision = "mutated-revision"
	put.Signature.DiagnosticSignatures["prompt"] = "mutated-prompt"
	put.Metadata["k"] = "mutated-metadata"

	got, ok, err := store.GetNode(ctx, "conversation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetNode ok = false, want true")
	}
	if !reflect.DeepEqual(got.ParentIDs, []NodeID{"parent-b", "parent-a"}) {
		t.Fatalf("ParentIDs = %v, want original unsorted order", got.ParentIDs)
	}
	if got.SourceRefs[0].Message.MessageID != "message-1" {
		t.Fatalf("SourceRefs message id = %q, want message-1", got.SourceRefs[0].Message.MessageID)
	}
	if got.Signature.SourceRevisions[0].Revision != "1" {
		t.Fatalf("Signature source revision = %q, want 1", got.Signature.SourceRevisions[0].Revision)
	}
	if len(got.Signature.UpstreamViewRefs) != 0 {
		t.Fatalf("Signature upstream refs = %+v, want none", got.Signature.UpstreamViewRefs)
	}
	if got.Signature.DiagnosticSignatures["prompt"] != "summary:v1" {
		t.Fatalf("Signature transform = %q, want summary:v1", got.Signature.DiagnosticSignatures["prompt"])
	}
	if got.Metadata["k"] != "v" {
		t.Fatalf("Metadata k = %q, want v", got.Metadata["k"])
	}

	listed, err := store.ListNodes(ctx, "conversation-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	listed[0].ParentIDs[0] = "mutated-listed-parent"
	got, _, err = store.GetNode(ctx, "conversation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentIDs[0] != "parent-b" {
		t.Fatalf("stored ParentIDs mutated through ListNodes result: %v", got.ParentIDs)
	}
}

func TestSummaryWorkspaceStoreListLevelAfterIDLimitDeterministic(t *testing.T) {
	ctx := context.Background()
	store := NewSummaryWorkspaceStore(workspace.NewMemWorkspace())
	levels := map[NodeID]int{
		"bravo":   1,
		"alpha":   0,
		"delta":   1,
		"charlie": 1,
	}
	for _, id := range []NodeID{"bravo", "alpha", "delta", "charlie"} {
		node := validNode("conversation-1", id)
		node.Level = levels[id]
		if _, err := store.PutNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.ListNodes(ctx, "conversation-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, all, []NodeID{"alpha", "bravo", "charlie", "delta"})

	levelOne := 1
	filtered, err := store.ListNodes(ctx, "conversation-1", ListOptions{AfterID: "alpha", Limit: 2, Level: &levelOne})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, filtered, []NodeID{"bravo", "charlie"})

	levelZero := 0
	onlyLevelZero, err := store.ListNodes(ctx, "conversation-1", ListOptions{Level: &levelZero})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, onlyLevelZero, []NodeID{"alpha"})

	missing, err := store.ListNodes(ctx, "missing-conversation", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("ListNodes missing conversation returned %d nodes, want 0", len(missing))
	}
}

func TestSummaryWorkspaceStorePathSafeIDsRoundTripAndTargetedDelete(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		conversationID string
		nodeID         NodeID
	}{
		{conversationID: ".", nodeID: "."},
		{conversationID: "..", nodeID: ".."},
		{conversationID: "conversation/with/slash", nodeID: "node/with/slash"},
		{conversationID: "name%percent", nodeID: "node%percent"},
		{conversationID: "suffix.json", nodeID: "node.json"},
	}

	for _, tc := range cases {
		t.Run(tc.conversationID+"/"+string(tc.nodeID), func(t *testing.T) {
			ws := workspace.NewMemWorkspace()
			store := NewSummaryWorkspaceStore(ws)
			if _, err := store.PutNode(ctx, validNode(tc.conversationID, tc.nodeID)); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PutNode(ctx, validNode(tc.conversationID, "sibling-node")); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PutNode(ctx, validNode("sentinel-conversation", "sentinel-node")); err != nil {
				t.Fatal(err)
			}

			conversationSegment := store.pathSegment(tc.conversationID)
			nodeSegment := store.pathSegment(string(tc.nodeID))
			assertSafeWorkspaceSegment(t, store, conversationSegment, tc.conversationID, "sdag_")
			assertSafeWorkspaceSegment(t, store, nodeSegment, string(tc.nodeID), "sdag_")

			encodedPath := "conversations/" + conversationSegment + "/nodes/" + nodeSegment + ".json"
			if exists, err := ws.Exists(ctx, encodedPath); err != nil || !exists {
				t.Fatalf("encoded node exists = %v err %v, want true nil", exists, err)
			}
			rawPath := "conversations/" + tc.conversationID + "/nodes/" + string(tc.nodeID) + ".json"
			if rawPath != encodedPath {
				if exists, err := ws.Exists(ctx, rawPath); err != nil || exists {
					t.Fatalf("raw node path %q exists = %v err %v, want false nil", rawPath, exists, err)
				}
			}

			got, ok, err := store.GetNode(ctx, tc.conversationID, tc.nodeID)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || got.ConversationID != tc.conversationID || got.ID != tc.nodeID {
				t.Fatalf("GetNode path-safe id = %+v ok %v, want original ids", got, ok)
			}

			if err := store.DeleteConversation(ctx, tc.conversationID); err != nil {
				t.Fatal(err)
			}
			if listed, err := store.ListNodes(ctx, tc.conversationID, ListOptions{}); err != nil || len(listed) != 0 {
				t.Fatalf("ListNodes after target delete = %d err %v, want 0 nil", len(listed), err)
			}
			kept, ok, err := store.GetNode(ctx, "sentinel-conversation", "sentinel-node")
			if err != nil {
				t.Fatal(err)
			}
			if !ok || kept.Summary != "summary for sentinel-node" {
				t.Fatalf("sentinel after DeleteConversation(%q) = %+v ok %v, want kept", tc.conversationID, kept, ok)
			}
		})
	}
}

func TestSummaryWorkspaceStoreCustomPathSegmentPrefix(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := NewSummaryWorkspaceStore(ws, WithSummaryPathSegmentPrefix("custom_"))
	node := validNode("conversation/with/slash", "node/with/slash")

	if _, err := store.PutNode(ctx, node); err != nil {
		t.Fatal(err)
	}

	conversationSegment := store.pathSegment(node.ConversationID)
	nodeSegment := store.pathSegment(string(node.ID))
	assertSafeWorkspaceSegment(t, store, conversationSegment, node.ConversationID, "custom_")
	assertSafeWorkspaceSegment(t, store, nodeSegment, string(node.ID), "custom_")

	encodedPath := "conversations/" + conversationSegment + "/nodes/" + nodeSegment + ".json"
	if exists, err := ws.Exists(ctx, encodedPath); err != nil || !exists {
		t.Fatalf("custom-prefixed node exists = %v err %v, want true nil", exists, err)
	}

	got, ok, err := store.GetNode(ctx, node.ConversationID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetNode ok = false, want true")
	}
	assertNodeEqual(t, got, node)

	listed, err := store.ListNodes(ctx, node.ConversationID, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, listed, []NodeID{node.ID})

	if err := store.DeleteConversation(ctx, node.ConversationID); err != nil {
		t.Fatal(err)
	}
	if listed, err := store.ListNodes(ctx, node.ConversationID, ListOptions{}); err != nil || len(listed) != 0 {
		t.Fatalf("ListNodes after custom-prefixed delete = %d err %v, want 0 nil", len(listed), err)
	}
}

func TestSummaryWorkspaceStoreMetadataJSONRoundTripSemantics(t *testing.T) {
	ctx := context.Background()
	store := NewSummaryWorkspaceStore(workspace.NewMemWorkspace())
	node := validNode("conversation-1", "node-1")
	node.Metadata = map[string]any{
		"int":    7,
		"bool":   true,
		"nested": map[string]any{"count": 2, "ok": false},
	}

	if _, err := store.PutNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetNode(ctx, "conversation-1", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetNode ok = false, want true")
	}

	if got.Metadata["int"] != float64(7) {
		t.Fatalf("metadata int = %#v, want float64(7)", got.Metadata["int"])
	}
	if got.Metadata["bool"] != true {
		t.Fatalf("metadata bool = %#v, want true", got.Metadata["bool"])
	}
	nested, ok := got.Metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata nested type = %T, want map[string]any", got.Metadata["nested"])
	}
	if nested["count"] != float64(2) || nested["ok"] != false {
		t.Fatalf("metadata nested = %#v, want count float64(2) and ok false", nested)
	}
}

func validNode(conversationID string, id NodeID) SummaryNode {
	created := time.Date(2026, 6, 9, 1, 2, 3, 0, time.UTC)
	updated := time.Date(2026, 6, 9, 4, 5, 6, 0, time.UTC)
	sourceRef := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: conversationID,
			MessageID:      "message-1",
			Span:           &views.Span{Start: 0, End: 10},
		},
	}
	return SummaryNode{
		ID:             id,
		ConversationID: conversationID,
		ParentIDs:      []NodeID{"parent-1"},
		SourceRefs:     []views.SourceRef{sourceRef},
		Summary:        "summary for " + string(id),
		Level:          1,
		Signature: views.ViewSignature{
			ViewID: views.ID("summary_dag"),
			SourceRevisions: []views.SourceRevision{{
				Kind:      views.SourceMessage,
				SourceKey: sourceRef.StableKey(),
				Revision:  "1",
			}},
			DiagnosticSignatures: map[string]string{"prompt": "summary:v1"},
		},
		CreatedAt: created,
		UpdatedAt: updated,
		Metadata:  map[string]any{"k": "v"},
	}
}

func assertNodeEqual(t *testing.T, got, want SummaryNode) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("node mismatch:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func assertNodeIDs(t *testing.T, nodes []SummaryNode, want []NodeID) {
	t.Helper()
	got := make([]NodeID, 0, len(nodes))
	for _, node := range nodes {
		got = append(got, node.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("node IDs = %v, want %v", got, want)
	}
}

func assertSafeWorkspaceSegment(t *testing.T, store *SummaryWorkspaceStore, segment, raw, wantPrefix string) {
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
