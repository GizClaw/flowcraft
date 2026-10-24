package memory

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/projection/bm25"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// laneBases counts the full snapshots one lane holds for one scope, whatever
// its manifest names: what the sweep is measured by.
func laneBases(t *testing.T, ws workspace.Workspace, family string) int {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := kvStore.Keys(context.Background(), path.Join("projections", family, "v2"))
	if err != nil {
		t.Fatal(err)
	}
	bases := 0
	for _, key := range keys {
		if strings.Contains(key, "/bases/") {
			bases++
		}
	}
	return bases
}

// TestMaintainReclaimsTheLanesSupersededBases is the wiring half: the sweep
// exists inside each lane, and the maintenance pass is what a host has without
// calling a lane directly. A lane compacts its own storage as it writes, so the
// generations it stopped serving are collected by the same pass that reclaims
// the superseded document builds -- or they are never collected at all.
func TestMaintainReclaimsTheLanesSupersededBases(t *testing.T) {
	ctx := context.Background()
	assembly, ws := newTestAssemblyWith(t, testSettings, WithDeriver(perInputDeriver{}))
	if err := assembly.CommitTurn(ctx, corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "the bm25 lane keeps this")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Publish the lane's state twice more under the name the assembly reads:
	// what a compaction does to the state it replaced. The publication keeps one
	// generation of grace, so the lane sits at two bases with one readable.
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	lane, err := bm25.New(bm25.Config{KV: kvStore, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	request := component.ProjectionRequest{Scope: testScope()}
	if err := lane.FullRebuild(ctx, request); err != nil {
		t.Fatal(err)
	}
	request.Artifacts = []component.Artifact{{
		Kind:     "fact",
		ID:       "fact-1",
		Content:  coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "a fact to keep"}}},
		Sources:  []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message-1"}},
		Metadata: corememory.Metadata{"conversation_id": "conv-1"},
	}}
	if err := lane.FullRebuild(ctx, request); err != nil {
		t.Fatal(err)
	}
	if bases := laneBases(t, ws, "bm25"); bases != 2 {
		t.Fatalf("bm25 bases before the pass = %d, want the active one and its grace", bases)
	}

	plan, err := assembly.Maintain(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if plan.ReclaimedProjectionObjects != 1 {
		t.Fatalf("the pass reclaimed %d objects, want the superseded base", plan.ReclaimedProjectionObjects)
	}
	if bases := laneBases(t, ws, "bm25"); bases != 1 {
		t.Fatalf("bm25 bases after the pass = %d, want the one the manifest names", bases)
	}

	// A second pass finds nothing to reclaim and reports the pass it saved.
	again, err := assembly.Maintain(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if again.ReclaimedProjectionObjects != 0 {
		t.Fatalf("second pass reclaimed %d objects", again.ReclaimedProjectionObjects)
	}
}
