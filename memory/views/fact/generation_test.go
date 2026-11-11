package fact

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// The generation is the identity of the derivation policy that produced a set
// of facts. These tests pin the two rules that let a policy change converge
// instead of accumulating: a later generation does not share a key with the one
// it replaces, and it stays invisible until a completed pass publishes it.
func TestFactStoreGenerationSwitchHidesTheReplacedGeneration(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	store := newFactStore(t, newTestWorkspace(t), WithClock(func() time.Time { return now }))
	const first, second = "policy-a", "policy-b"

	// The same text under the same fact ID: a re-derivation reuses content
	// identities, so the second generation must not collide with the first.
	shared := factRequest("fact-1", "Alice likes tea")
	shared.Generation = first
	if _, err := store.Add(ctx, shared); err != nil {
		t.Fatal(err)
	}
	active, found, err := store.ActiveGeneration(ctx, factScope, "conversation")
	if err != nil || !found || active != first {
		t.Fatalf("active generation = %q, %v, %v", active, found, err)
	}

	replacement := factRequest("fact-1", "Alice likes tea")
	replacement.Generation = second
	replacement.Provenance = []corememory.SourceRef{{
		Kind: corememory.SourceMessage, ID: "message-2", Revision: "2",
	}}
	if _, err := store.Add(ctx, replacement); err != nil {
		t.Fatalf("same identity in another generation must not conflict: %v", err)
	}
	added := factRequest("fact-2", "Alice likes coffee")
	added.Generation = second
	if _, err := store.Add(ctx, added); err != nil {
		t.Fatal(err)
	}

	// Nothing published the second generation yet, so readers still see the
	// first one only: an unfinished re-derivation is never served.
	visible, err := store.List(ctx, factScope, "conversation", ListOptions{})
	if err != nil || len(visible) != 1 || visible[0].Text != "Alice likes tea" {
		t.Fatalf("visible before publish = %#v, %v", visible, err)
	}
	if _, ok, err := store.Get(ctx, factScope, "conversation", "fact-2"); err != nil || ok {
		t.Fatalf("unpublished fact is readable: %v, %v", ok, err)
	}

	if err := store.PublishActiveGeneration(ctx, factScope, "conversation", second); err != nil {
		t.Fatal(err)
	}
	visible, err = store.List(ctx, factScope, "conversation", ListOptions{})
	if err != nil || len(visible) != 2 {
		t.Fatalf("visible after publish = %#v, %v", visible, err)
	}
	for _, value := range visible {
		if value.Generation != second {
			t.Fatalf("replaced generation leaked into reads: %#v", value)
		}
	}
	scopeWide, err := store.ListScope(ctx, factScope)
	if err != nil || len(scopeWide) != 2 {
		t.Fatalf("scope scan = %#v, %v", scopeWide, err)
	}
	for _, value := range scopeWide {
		if value.Generation != second {
			t.Fatalf("scope scan leaked the replaced generation: %#v", value)
		}
	}

	// The derivation side keeps reading the generation it is building.
	previous, err := store.List(ctx, factScope, "conversation", ListOptions{Generation: first})
	if err != nil || len(previous) != 1 || previous[0].Generation != first {
		t.Fatalf("explicit generation read = %#v, %v", previous, err)
	}
	// Derivation never reached across generations: writing the same identity
	// into the new generation left the replaced generation's record untouched
	// instead of merging the two policies' provenance together.
	if len(previous[0].Provenance) != 1 || previous[0].Provenance[0].ID != "message-1" {
		t.Fatalf("replaced generation was merged into: %#v", previous[0].Provenance)
	}
	if current, ok, err := store.Get(ctx, factScope, "conversation", "fact-1"); err != nil || !ok ||
		len(current.Provenance) != 1 || current.Provenance[0].ID != "message-2" {
		t.Fatalf("active fact = %#v, %v, %v", current.Provenance, ok, err)
	}

	generations, err := store.ListGenerations(ctx, factScope, "conversation")
	if err != nil || !reflect.DeepEqual(generations, []string{first, second}) {
		t.Fatalf("generations = %#v, %v", generations, err)
	}
}

// A conversation with no published generation adopts the first one written, so
// a fresh conversation stays readable while it is derived. Only the first: the
// generation that replaces it waits for its own pass to complete.
func TestFactStoreBootstrapPublishesOnlyTheFirstGeneration(t *testing.T) {
	ctx := context.Background()
	store := newFactStore(t, newTestWorkspace(t), WithClock(func() time.Time {
		return time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	}))

	first := factRequest("fact-1", "first policy")
	first.Generation = "policy-a"
	if _, err := store.Add(ctx, first); err != nil {
		t.Fatal(err)
	}
	if active, found, err := store.ActiveGeneration(ctx, factScope, "conversation"); err != nil || !found || active != "policy-a" {
		t.Fatalf("bootstrap pointer = %q, %v, %v", active, found, err)
	}

	second := factRequest("fact-2", "second policy")
	second.Generation = "policy-b"
	if _, err := store.Add(ctx, second); err != nil {
		t.Fatal(err)
	}
	if active, _, err := store.ActiveGeneration(ctx, factScope, "conversation"); err != nil || active != "policy-a" {
		t.Fatalf("pointer followed the second generation: %q, %v", active, err)
	}
	visible, err := store.List(ctx, factScope, "conversation", ListOptions{})
	if err != nil || len(visible) != 1 || visible[0].Generation != "policy-a" {
		t.Fatalf("visible = %#v, %v", visible, err)
	}
}

// Republishing the active generation is a no-op, so a pass that finds nothing
// to derive does not rewrite the pointer.
func TestFactStoreRepublishingTheActiveGenerationKeepsThePointer(t *testing.T) {
	ctx := context.Background()
	published := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	now := published
	store := newFactStore(t, newTestWorkspace(t), WithClock(func() time.Time { return now }))
	if err := store.PublishActiveGeneration(ctx, factScope, "conversation", "policy-a"); err != nil {
		t.Fatal(err)
	}
	now = published.Add(time.Hour)
	if err := store.PublishActiveGeneration(ctx, factScope, "conversation", "policy-a"); err != nil {
		t.Fatal(err)
	}
	key, err := store.activeKey(factScope, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var active activeGeneration
	if err := decodeJSON(data, &active); err != nil {
		t.Fatal(err)
	}
	if !active.PublishedAt.Equal(published) {
		t.Fatalf("republish rewrote the pointer at %v", active.PublishedAt)
	}
}

// A generation is an opaque identity, but it still has to name a storage
// subtree: an empty, padded, or oversized one is rejected instead of being
// silently normalized onto another generation's key.
func TestFactStoreGenerationIdentityIsValidated(t *testing.T) {
	ctx := context.Background()
	store := newFactStore(t, newTestWorkspace(t))
	requests := []AddRequest{
		factRequest("empty", "text"),
		factRequest("padded", "text"),
		factRequest("trailing", "text"),
		factRequest("oversized", "text"),
	}
	requests[0].Generation = ""
	requests[1].Generation = " policy-a"
	requests[2].Generation = "policy-a "
	requests[3].Generation = strings.Repeat("a", maxGenerationBytes+1)
	for index, request := range requests {
		if _, err := store.Add(ctx, request); err == nil {
			t.Fatalf("request %d: generation %q accepted", index, request.Generation)
		}
		if err := store.PublishActiveGeneration(ctx, factScope, "conversation", request.Generation); err == nil {
			t.Fatalf("request %d: generation %q published", index, request.Generation)
		}
	}
	// An empty selection is not an identity: it asks for the active generation,
	// so it reads an empty conversation instead of failing.
	if values, err := store.List(ctx, factScope, "conversation", ListOptions{}); err != nil || len(values) != 0 {
		t.Fatalf("empty selection = %#v, %v", values, err)
	}
	for index, request := range requests[1:] {
		if _, err := store.List(ctx, factScope, "conversation", ListOptions{Generation: request.Generation}); err == nil {
			t.Fatalf("request %d: generation %q selected for reading", index, request.Generation)
		}
	}
}

// The pointer carries its own address, so a record written for another
// conversation cannot redirect this one's reads.
func TestFactStoreRejectsForeignActiveGeneration(t *testing.T) {
	ctx := context.Background()
	store := newFactStore(t, newTestWorkspace(t))
	if _, err := store.Add(ctx, factRequest("fact", "text")); err != nil {
		t.Fatal(err)
	}
	key, err := store.activeKey(factScope, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(activeGeneration{
		SchemaVersion: activeGenerationSchemaVersion, Generation: factGeneration,
		RuntimeID: factScope.RuntimeID, UserID: factScope.UserID, AgentID: factScope.AgentID,
		ConversationID: "other", PublishedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, factScope, "conversation", ListOptions{}); err == nil {
		t.Fatal("foreign pointer accepted")
	}
}
