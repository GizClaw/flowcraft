package worker

import (
	"context"
	"errors"
	"testing"

	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// laneIDs lists the lane addresses one generation's facts are projected under,
// sorted the way the purge builds them.
func laneIDs(t *testing.T, store *generationStore, scope corememory.Scope, conversationID, generation string) []string {
	t.Helper()
	facts, err := store.facts.List(context.Background(), scope, conversationID, factview.ListOptions{Generation: generation})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(facts))
	for _, fact := range facts {
		ids = append(ids, factLaneID(conversationID, fact.ID))
	}
	return ids
}

// TestPurgeGenerationsDropsTheEntriesOfAGenerationThatNeverPublished pins the
// reason retention sweeps the lanes before it sweeps the facts: a generation a
// failed converge left stored and projected has entries no later pass can
// enumerate -- the converge walks the stored generations -- so a sweep that
// retired its facts first would leave the lanes offering a candidate the read
// path cannot hydrate, for good.
func TestPurgeGenerationsDropsTheEntriesOfAGenerationThatNeverPublished(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: {conversationCommit(conversationID, "we talked about beverages")},
	}}
	checkpoints := newMemoryCheckpoints()
	lane := newRecordingLane()
	store := newGenerationStore(t)

	published := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	if _, err := published.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	kept := lane.projected(conversationID)
	if len(kept) != 1 {
		t.Fatalf("projected entries = %#v, want one", kept)
	}

	abandoned := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		policyDeriver{prefix: "b"}, lane)
	lane.failNextConverge = errors.New("lane is down")
	if _, err := abandoned.ProcessConversation(ctx, scope, conversationID); err == nil {
		t.Fatal("a failed converge was reported as a successful pass")
	}
	if projected := lane.projected(conversationID); len(projected) != 2 {
		t.Fatalf("projected entries after the failed pass = %#v, want the abandoned generation's beside the published one", projected)
	}
	dropped := laneIDs(t, store, scope, conversationID, "policy-b")
	if len(dropped) != 1 {
		t.Fatalf("the abandoned generation projected %#v, want one entry", dropped)
	}

	// The sweep drops what the generation that never published left in the
	// lanes, and keeps the published generation's entries.
	if entries, err := abandoned.PurgeGenerations(ctx, scope, conversationID, []string{"policy-b"}); err != nil || entries != 1 {
		t.Fatalf("purge = %d, %v, want 1", entries, err)
	}
	if projected := lane.projected(conversationID); len(projected) != 1 || projected[0] != kept[0] {
		t.Fatalf("projected entries after the purge = %#v, want %#v", projected, kept)
	}

	// Once the facts are gone the purge has nothing left to enumerate: the same
	// call is a no-op rather than an error, so a sweep interrupted between the
	// two retries from wherever it stopped.
	if retired, err := store.facts.RetireGenerations(ctx, scope, conversationID); err != nil || retired == 0 {
		t.Fatalf("retire = %d, %v, want the abandoned generation's facts", retired, err)
	}
	if entries, err := abandoned.PurgeGenerations(ctx, scope, conversationID, []string{"policy-b"}); err != nil || entries != 0 {
		t.Fatalf("purge after retirement = %d, %v, want 0", entries, err)
	}
	if projected := lane.projected(conversationID); len(projected) != 1 || projected[0] != kept[0] {
		t.Fatalf("projected entries after retirement = %#v, want %#v", projected, kept)
	}
}

// TestPurgeGenerationsKeepsTheEntriesTwoGenerationsShare pins what the purge
// does not do: a fact is a content address, so a generation that re-derived the
// same content keeps projecting it, and retirement never prunes an entry the
// generation readers resolve still owns.
func TestPurgeGenerationsKeepsTheEntriesTwoGenerationsShare(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: {conversationCommit(conversationID, "we talked about beverages")},
	}}
	checkpoints := newMemoryCheckpoints()
	lane := newRecordingLane()
	store := newGenerationStore(t)

	// Both policies derive the same text, so both generations own the same
	// fact: the second pass switches, and the entry stays projected for both.
	first := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	if _, err := first.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	second := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		policyDeriver{prefix: "a"}, lane)
	if _, err := second.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	shared := lane.projected(conversationID)
	if len(shared) != 1 {
		t.Fatalf("projected entries = %#v, want the fact both generations derive", shared)
	}

	if entries, err := second.PurgeGenerations(ctx, scope, conversationID, []string{"policy-a"}); err != nil || entries != 0 {
		t.Fatalf("purge of a shared fact = %d, %v, want 0", entries, err)
	}
	if projected := lane.projected(conversationID); len(projected) != 1 || projected[0] != shared[0] {
		t.Fatalf("projected entries after the purge = %#v, want %#v", projected, shared)
	}
}
