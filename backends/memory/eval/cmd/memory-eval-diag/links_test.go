package main

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// fakeFactStore stands in for *factview.FactStore: the walk reads records by
// id, and a test needs no workspace to answer that.
type fakeFactStore map[string]factview.Fact

func (fake fakeFactStore) Get(_ context.Context, _ corememory.Scope, conversationID, factID string) (factview.Fact, bool, error) {
	record, ok := fake[conversationID+"/"+factID]
	return record, ok, nil
}

// fakeProvenance stands in for the message provenance resolver, keyed by the
// item id the matcher caches under.
type fakeProvenance map[string][]eval.ResolvedSource

func (fake fakeProvenance) ResolveSources(_ context.Context, item corememory.ContextItem) []eval.ResolvedSource {
	return fake[item.ID]
}

func TestLinkWalkReachesOneHopOnly(t *testing.T) {
	const conversation = "sample-1"
	facts := fakeFactStore{
		"sample-1/fact-a": {ID: "fact-a", LinkedMemoryIDs: []string{"fact-b", "fact-b", "fact-gone"}},
		"sample-1/fact-b": {ID: "fact-b", LinkedMemoryIDs: []string{"fact-c"}, Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "sample-1/m2"}}},
		"sample-1/fact-c": {ID: "fact-c", Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "sample-1/m3"}}},
	}
	provenance := fakeProvenance{
		"fact-b": {{TurnID: "D1:2", Text: "Bob: I adopted a dog."}},
		"fact-c": {{TurnID: "D1:3", Text: "Bob: And a cat too."}},
	}
	walk := newLinkWalk(facts, eval.NewMatcher(provenance), corememory.Scope{RuntimeID: "eval"})
	ctx := context.Background()
	pool := []corememory.ContextItem{
		{ID: "m1", Kind: corememory.ContextRawMessage},
		{ID: "fact-a", Kind: corememory.ContextFact},
	}
	targets := walk.neighborhood(ctx, conversation, pool)
	if len(targets) != 1 || targets[0].ID != "fact-b" {
		t.Fatalf("targets = %#v, want just fact-b", targets)
	}
	if coverage := walk.matcher.Cover(ctx, targets, "D1:2", "Bob: I adopted a dog."); !coverage.Covered() {
		t.Fatal("a fact one hop from the pool must count as reached")
	}
	if coverage := walk.matcher.Cover(ctx, targets, "D1:9", "something else"); coverage.Covered() {
		t.Fatal("a turn outside the walk must not count")
	}
	if coverage := walk.matcher.Cover(ctx, targets, "D1:3", "Bob: And a cat too."); coverage.Covered() {
		t.Fatal("two hops out must not count: the walk is one hop, not transitive")
	}
	if walk.reads != 3 || walk.unresolved != 1 {
		t.Fatalf("reads=%d unresolved=%d, want 3 and 1", walk.reads, walk.unresolved)
	}
}

// A store ingested before ingest tagged messages carries no dataset turn ids,
// and then the walk has only committed text to stand a turn on.
func TestLinkWalkFallsBackToTextWhenUntagged(t *testing.T) {
	const conversation = "sample-1"
	facts := fakeFactStore{
		"sample-1/fact-a": {ID: "fact-a", LinkedMemoryIDs: []string{"fact-b"}},
		"sample-1/fact-b": {ID: "fact-b", Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "sample-1/m2"}}},
	}
	provenance := fakeProvenance{"fact-b": {{Text: "Alice: I like tea."}}}
	walk := newLinkWalk(facts, eval.NewMatcher(provenance), corememory.Scope{RuntimeID: "eval"})
	ctx := context.Background()
	targets := walk.neighborhood(ctx, conversation, []corememory.ContextItem{{ID: "fact-a", Kind: corememory.ContextFact}})
	if coverage := walk.matcher.Cover(ctx, targets, "D1:1", "Alice: I like tea."); !coverage.Covered() {
		t.Fatal("an untagged store must be stood on by committed text")
	}
	if coverage := walk.matcher.Cover(ctx, targets, "D1:1", "Alice: I like coffee."); coverage.Covered() {
		t.Fatal("a turn the record's text does not hold must not count")
	}
}
