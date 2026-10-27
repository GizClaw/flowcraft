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
	keys, err := kvStore.Keys(context.Background(), path.Join("projections", family, "v3"))
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

// assemblyWithSupersededLane builds an assembly whose bm25 lane holds two
// bases: the one its manifest names and the grace generation the publication
// that replaced it spared. Everything the two tests below sweep is this state.
func assemblyWithSupersededLane(t *testing.T) (*Assembly, workspace.Workspace, *bm25.Index) {
	t.Helper()
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
	request := component.ProjectionRequest{Scope: testScope(), Artifacts: []component.Artifact{{
		Kind:     "fact",
		ID:       "fact-0",
		Content:  coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "a fact to replace"}}},
		Sources:  []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message-0"}},
		Metadata: corememory.Metadata{"conversation_id": "conv-1"},
	}}}
	if err := lane.FullRebuild(ctx, request); err != nil {
		t.Fatal(err)
	}
	// A rebuild replaces the shard it publishes: an empty artifact set would
	// drop the shard instead of leaving a generation behind, and the state this
	// test sweeps is a publication that replaced another.
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
		t.Fatalf("bm25 bases before the sweep = %d, want the active one and its grace", bases)
	}
	return assembly, ws, lane
}

// TestMaintainReclaimsTheLanesSupersededBases is the wiring half: the sweep
// exists inside each lane, and the maintenance pass is what a host has without
// calling a lane directly. A lane compacts its own storage as it writes, so the
// generations it stopped serving are collected by the same pass that reclaims
// the superseded document builds -- or they are never collected at all.
func TestMaintainReclaimsTheLanesSupersededBases(t *testing.T) {
	ctx := context.Background()
	assembly, ws, _ := assemblyWithSupersededLane(t)

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

// TestReclaimProjectionsIsTheSweepWithoutThePass is the host-facing half: an
// operator holding a workspace an earlier build filled asks for the storage no
// manifest names, not for judgments over facts -- a pass is what they would
// have to pay for decisions they are not making. What it removes is the same
// state the pass removes, and what it leaves still resolves.
func TestReclaimProjectionsIsTheSweepWithoutThePass(t *testing.T) {
	ctx := context.Background()
	assembly, ws, lane := assemblyWithSupersededLane(t)

	result, err := assembly.ReclaimProjections(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if result.Bases+result.Segments != 1 {
		t.Fatalf("swept %+v, want the superseded base", result)
	}
	// The lane that swept and the one that found nothing are both visited:
	// testSettings wires bm25 and entity, and neither stores outside its
	// manifest.
	if result.Lanes != 2 {
		t.Fatalf("lanes swept = %d, want bm25 and entity", result.Lanes)
	}
	if bases := laneBases(t, ws, "bm25"); bases != 1 {
		t.Fatalf("bm25 bases after the sweep = %d, want the one the manifest names", bases)
	}
	// The state the sweep kept is the state the read path resolves: a sweep
	// that deleted a referenced object would leave a lane that cannot
	// materialize.
	source, computedSource, buildDigest, computedBuild, found, err := lane.AuditDigests(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if !found || source != computedSource || buildDigest != computedBuild {
		t.Fatalf("lane audit after the sweep: found=%t source=%q/%q build=%q/%q",
			found, source, computedSource, buildDigest, computedBuild)
	}
}
