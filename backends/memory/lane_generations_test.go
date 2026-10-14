package memory

import (
	"context"
	"testing"

	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// seedLaneName writes one lane's state under a name this deployment does not
// read: what a lane whose storage name is a function of its producer identity
// leaves behind when that identity changes, still on disk and unreachable.
func seedLaneName(t *testing.T, ws workspace.Workspace, family, name string, scope corememory.Scope) {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := projectionstore.NewTypedStore[map[string]bool, map[string]bool](
		kvStore, family,
		projectionstore.TypedOptions[map[string]bool, map[string]bool]{
			ProducerVersion: "older-producer",
			Apply: func(base *map[string]bool, delta map[string]bool) error {
				for key, value := range delta {
					(*base)[key] = value
				}
				return nil
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FullRebuild(
		context.Background(), scope, name, map[string]bool{"legacy": true}, "legacy-source"); err != nil {
		t.Fatal(err)
	}
}

// A lane that reads a name nothing was ever written under has no manifest to
// disagree with, so it reports what a scope that was never indexed reports:
// nothing stored, not stale. The state an earlier generation holds under the
// name it did write is what says the difference, and that is what tells a host
// to repair the lane.
func TestLaneStatusReportsStateUnderAnotherName(t *testing.T) {
	ctx := context.Background()
	assembly, ws, _ := derivedLaneAssembly(t)

	// The scope this deployment derived: the lane reads its own name, and the
	// name it does not read is reported beside it.
	seedLaneName(t, ws, "bm25", "facts-legacy", testScope())
	lanes, err := assembly.LaneStatus(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	bm25 := laneNamed(t, lanes, "bm25")
	if !bm25.Stored || bm25.Stale {
		t.Fatalf("the lane that reads its own name reported %+v", bm25)
	}
	if len(bm25.OtherGenerations) != 1 || bm25.OtherGenerations[0] != "facts-legacy" {
		t.Fatalf("bm25 other generations = %v, want [facts-legacy]", bm25.OtherGenerations)
	}
	for _, lane := range lanes {
		if lane.Name == "bm25" {
			continue
		}
		if len(lane.OtherGenerations) != 0 {
			t.Fatalf("lane %q reported state under another name: %v", lane.Name, lane.OtherGenerations)
		}
	}

	// The scope a bumped identity reads: nothing under the name the lane
	// writes, the earlier generation under a name beside it.
	forked := corememory.Scope{RuntimeID: "runtime-forked"}
	seedLaneName(t, ws, "bm25", "facts-legacy", forked)
	lanes, err = assembly.LaneStatus(ctx, forked)
	if err != nil {
		t.Fatal(err)
	}
	forkedLane := laneNamed(t, lanes, "bm25")
	if forkedLane.Stored || forkedLane.Stale {
		t.Fatalf("a lane reading an unwritten name reported %+v", forkedLane)
	}
	if len(forkedLane.OtherGenerations) != 1 || forkedLane.OtherGenerations[0] != "facts-legacy" {
		t.Fatalf("forked lane other generations = %v, want [facts-legacy]", forkedLane.OtherGenerations)
	}

	// The assembly-wide snapshot carries the same report per scope, which is
	// where a host that does not know which scope to ask looks. Only the
	// derived scope is registered, so the report comes from its lanes.
	diagnostics, err := assembly.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.Scopes) != 1 {
		t.Fatalf("diagnostics scopes = %+v", diagnostics.Scopes)
	}
	if seeded := laneNamed(t, diagnostics.Scopes[0].Lanes, "bm25"); len(seeded.OtherGenerations) != 1 ||
		seeded.OtherGenerations[0] != "facts-legacy" {
		t.Fatalf("assembly diagnostics reported %v", seeded.OtherGenerations)
	}
}
