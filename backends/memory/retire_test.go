package memory

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
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
	// The lanes are swept with the views: the entries of the replaced
	// generation are the ones its switch pruned, and the sweep drops them
	// again so a lane that missed the converge is reconciled too.
	if want := (RetireResult{Facts: len(stored), Summaries: 1, LaneEntries: len(stored)}); retired != want {
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

// failingDeriver derives exactly like the replacing deriver until its first
// calls succeed, then fails. A pass that fails there has already stored and
// projected what the commits before the failure derived, and never reaches the
// publication that would have made its generation the one readers resolve.
type failingDeriver struct {
	deriver  replacingDeriver
	succeeds int
	calls    int
}

func (deriver *failingDeriver) Derive(ctx context.Context, input component.Artifact) ([]component.Artifact, error) {
	deriver.calls++
	if deriver.calls > deriver.succeeds {
		return nil, errors.New("memory: derivation is unavailable")
	}
	return deriver.deriver.Derive(ctx, input)
}

// TestRetiringGenerationsDropsTheEntriesOfAGenerationNoPassPublished is the
// acceptance test for sweeping the projection lanes with the views: a pass that
// fails after projecting leaves a generation whose facts are stored and whose
// entries are in the lanes, and which no switch ever reconciled because it never
// published. Retiring its facts while those entries stay projected leaves a
// candidate every later query has to fetch and fail to hydrate -- the lanes walk
// the stored generations, so a retired one's entries can never be enumerated
// again -- and nothing in the module would ever collect it.
func TestRetiringGenerationsDropsTheEntriesOfAGenerationNoPassPublished(t *testing.T) {
	ctx, ws := retirementFixture(t)
	published := deriveGeneration(t, ctx, ws, "a", "policy-a")
	publishedDigest := published.PolicyDigest()
	if err := published.Close(); err != nil {
		t.Fatal(err)
	}

	// The second commit is never derived, so the pass stops between what it
	// projected and the generation it would have published.
	abandoned := newTestAssemblyOn(t, ws, generationSettings,
		WithDeriver(&failingDeriver{deriver: replacingDeriver{name: "b"}, succeeds: 1}),
		WithDeriverVersion("policy-b"))
	abandonedDigest := abandoned.PolicyDigest()
	if abandonedDigest == publishedDigest {
		t.Fatal("the second policy kept the previous digest")
	}
	if err := abandoned.RunOnce(ctx); err == nil {
		t.Fatal("the failing pass reported success")
	}
	if active, found, err := abandoned.Facts().ActiveGeneration(ctx, testScope(), "conv-1"); err != nil || !found || active != publishedDigest {
		t.Fatalf("active generation after the failing pass = %q, %v, %v", active, found, err)
	}
	assertStoredGenerations(t, ctx, abandoned, publishedDigest, abandonedDigest)
	derived, err := abandoned.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{Generation: abandonedDigest})
	if err != nil || len(derived) != 1 {
		t.Fatalf("facts of the generation that never published = %#v, %v", derived, err)
	}
	// Compaction publishes the generation it summarises, so the branch followed
	// the failing pass; a later pass moves it back.
	if manifest, found, err := abandoned.Summaries().LoadActive(ctx, testScope(), "conv-1"); err != nil || !found || manifest.GenerationID != abandonedDigest {
		t.Fatalf("active manifest after the failing pass = %#v, %v, %v", manifest, found, err)
	}
	if err := abandoned.Close(); err != nil {
		t.Fatal(err)
	}

	// A pass under the published policy again derives nothing -- its watermark
	// is at the end of the stream -- and serves its own summaries again.
	current := deriveGeneration(t, ctx, ws, "a", "policy-a")
	if manifest, found, err := current.Summaries().LoadActive(ctx, testScope(), "conv-1"); err != nil || !found || manifest.GenerationID != publishedDigest {
		t.Fatalf("active manifest after the recovery pass = %#v, %v, %v", manifest, found, err)
	}

	retired, err := current.RetireGenerations(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (RetireResult{Facts: 1, Summaries: 1, LaneEntries: 1}); retired != want {
		t.Fatalf("retired = %#v, want %#v", retired, want)
	}
	assertStoredGenerations(t, ctx, current, publishedDigest)

	// The read path serves the published generation and nothing else: the sweep
	// dropped a candidate that could not be hydrated, not the window it belongs
	// to.
	result, err := current.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "beverages",
	})
	if err != nil {
		t.Fatal(err)
	}
	visible := assertVisibleFacts(t, current, publishedDigest, "a")
	served := 0
	for _, item := range result.Items {
		if item.Kind != corememory.ContextFact {
			continue
		}
		served++
		known := false
		for _, fact := range visible {
			if fact.ID == item.ID {
				known = true
				break
			}
		}
		if !known {
			t.Fatalf("served fact %q belongs to the generation that never published", item.ID)
		}
	}
	if served != 1 {
		t.Fatalf("served fact items = %d, want 1", served)
	}
	if diagnostics := current.provider.LastDiagnostics(); len(diagnostics) != 0 {
		t.Fatalf("the sweep left entries the read path cannot hydrate: %#v", diagnostics)
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
