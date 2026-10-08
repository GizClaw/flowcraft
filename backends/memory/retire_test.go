package memory

import (
	"context"
	"reflect"
	"sort"
	"testing"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// TestRetiringGenerationsDropsGenerationsNoViewServes is the acceptance test for
// retention across the derived views: one sweep takes a generation out of every
// view it left something in, so no read path serves the facts it removed and the
// branch cannot be pointed back at a generation that is no longer there.
func TestRetiringGenerationsDropsGenerationsNoViewServes(t *testing.T) {
	ctx, ws := retirementFixture(t)
	replaced := deriveGeneration(t, ctx, ws, "a", "policy-a")
	replacedDigest := replaced.PolicyDigest()
	if err := replaced.Close(); err != nil {
		t.Fatal(err)
	}
	current := deriveGeneration(t, ctx, ws, "b", "policy-b")
	currentDigest := current.PolicyDigest()
	assertVisibleFacts(t, current, currentDigest, "b")

	// Both views hold both generations: the facts each derived, and the summary
	// bookmark each published while it was current.
	assertStoredGenerations(t, ctx, current, replacedDigest, currentDigest)
	stored, err := current.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{Generation: replacedDigest})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 {
		t.Fatal("the replaced generation left no facts to retire")
	}

	// An empty keep list is the sweep that drops everything but what the views
	// serve.
	retired, err := current.RetireGenerations(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (RetireResult{Facts: len(stored), Summaries: 1}); retired != want {
		t.Fatalf("retired = %#v, want %#v", retired, want)
	}
	assertStoredGenerations(t, ctx, current, currentDigest)
	if facts, err := current.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{Generation: replacedDigest}); err != nil || len(facts) != 0 {
		t.Fatalf("the retired generation is still readable: %#v, %v", facts, err)
	}
	// The summary records of the retired generation stay stored -- a record is
	// a content address shared between generations -- but nothing serves them.
	if records, err := current.Summaries().List(ctx, testScope(), "conv-1", summaryview.ListOptions{GenerationID: replacedDigest}); err != nil || len(records) == 0 {
		t.Fatalf("records of the retired generation = %#v, %v", records, err)
	}

	// Rolling back to a retired generation is not a rollback: there is no
	// manifest left to serve, so the branch stays on the generation it reads.
	if manifest, found, err := current.Summaries().PublishGeneration(ctx, testScope(), "conv-1", replacedDigest); err != nil || found {
		t.Fatalf("rollback to a retired generation = %#v, %v, %v", manifest, found, err)
	}
	if manifest, found, err := current.Summaries().LoadActive(ctx, testScope(), "conv-1"); err != nil || !found || manifest.GenerationID != currentDigest {
		t.Fatalf("active manifest = %#v, %v, %v", manifest, found, err)
	}
	plan, err := current.Verify(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("verify actions after the sweep = %#v", plan.Actions)
	}

	// The kept generation is untouched: its facts, its summaries, and its
	// manifest.
	assertVisibleFacts(t, current, currentDigest, "b")
	if items, err := countSummaryItems(current); err != nil || items == 0 {
		t.Fatalf("summary items of the kept generation = %d, %v", items, err)
	}
	// Sweeping again is the normal case: what was retired is absent, not an
	// error.
	if retired, err := current.RetireGenerations(ctx, testScope(), "conv-1"); err != nil || retired != (RetireResult{}) {
		t.Fatalf("retry = %#v, %v", retired, err)
	}
}

// TestRetiringGenerationsKeepsTheGenerationEitherViewServes pins the safety
// property that makes the sweep one action: a policy change can leave the facts
// on one generation and the summary branch on another, and a sweep in that
// window must not drop the generation either view is still serving -- dropping
// the facts a served manifest summarises is exactly the state the sweep exists
// to prevent.
func TestRetiringGenerationsKeepsTheGenerationEitherViewServes(t *testing.T) {
	ctx, ws := retirementFixture(t)
	replaced := deriveGeneration(t, ctx, ws, "a", "policy-a")
	replacedDigest := replaced.PolicyDigest()
	if err := replaced.Close(); err != nil {
		t.Fatal(err)
	}
	current := deriveGeneration(t, ctx, ws, "b", "policy-b")
	currentDigest := current.PolicyDigest()

	// The summary branch goes back to the generation the facts left behind, as
	// a rollback does; the fact pointer stays on the new one.
	if manifest, found, err := current.Summaries().PublishGeneration(ctx, testScope(), "conv-1", replacedDigest); err != nil || !found {
		t.Fatalf("rollback = %#v, %v, %v", manifest, found, err)
	}
	retired, err := current.RetireGenerations(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if retired != (RetireResult{}) {
		t.Fatalf("a sweep of disagreeing views retired = %#v", retired)
	}
	assertStoredGenerations(t, ctx, current, replacedDigest, currentDigest)
	// The views still serve what they served before the sweep: the facts of the
	// generation the pointer names, and the manifest the branch was rolled back
	// to with the records of that generation.
	assertVisibleFacts(t, current, currentDigest, "b")
	if manifest, records, found, err := current.Summaries().ActiveSnapshot(ctx, testScope(), "conv-1"); err != nil ||
		!found || manifest.GenerationID != replacedDigest || len(records) == 0 {
		t.Fatalf("served summaries = generation %q with %d records, %v, %v", manifest.GenerationID, len(records), found, err)
	}

	// With the branch back on the facts' generation, the same sweep has nothing
	// left that serves the replaced one and drops it whole.
	if manifest, found, err := current.Summaries().PublishGeneration(ctx, testScope(), "conv-1", currentDigest); err != nil || !found {
		t.Fatalf("publish of the current generation = %#v, %v, %v", manifest, found, err)
	}
	if retired, err = current.RetireGenerations(ctx, testScope(), "conv-1"); err != nil || retired.Summaries != 1 || retired.Facts == 0 {
		t.Fatalf("sweep = %#v, %v", retired, err)
	}
	assertStoredGenerations(t, ctx, current, currentDigest)
	if plan, err := current.Verify(ctx, testScope(), "conv-1"); err != nil || len(plan.Actions) != 0 {
		t.Fatalf("verify actions after the sweep = %#v, %v", plan.Actions, err)
	}
}

// retirementFixture returns a context and a workspace both derived generations
// are built on.
func retirementFixture(t *testing.T) (context.Context, workspace.Workspace) {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return context.Background(), ws
}

// assertStoredGenerations checks that both derived views hold exactly the
// generations named, and nothing else.
func assertStoredGenerations(t *testing.T, ctx context.Context, assembly *Assembly, want ...string) {
	t.Helper()
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	facts, err := assembly.Facts().ListGenerations(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(facts, sorted) {
		t.Fatalf("fact generations = %#v, want %#v", facts, sorted)
	}
	summaries, err := assembly.Summaries().ListGenerations(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summaries, sorted) {
		t.Fatalf("summary generations = %#v, want %#v", summaries, sorted)
	}
}
