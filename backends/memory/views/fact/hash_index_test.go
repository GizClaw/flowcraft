package fact

import (
	"context"
	"reflect"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func TestFactStoreRecordsTheCanonicalIdentityWithTheFact(t *testing.T) {
	ctx := context.Background()
	store := newFactStore(t, newTestWorkspace(t))
	if _, err := store.Add(ctx, factRequest("a", "first")); err != nil {
		t.Fatal(err)
	}
	canonicalHash := CanonicalHash(NormalizeText("first"))
	keys, err := store.indexedFactIDs(ctx, factScope, "conversation", factGeneration, canonicalHash)
	if err != nil || !reflect.DeepEqual(keys, []string{"a"}) {
		t.Fatalf("identities = %v, %v; want [a]", keys, err)
	}
	complete, err := store.hashIndexComplete(ctx, factScope, "conversation", factGeneration)
	if err != nil || !complete {
		t.Fatalf("identity area complete = %v, %v; want true", complete, err)
	}
	// The identity keys share the generation subtree with the facts and must
	// not surface as facts, nor as extra generations.
	values, err := store.List(ctx, factScope, "conversation", ListOptions{})
	if err != nil || len(values) != 1 || values[0].ID != "a" {
		t.Fatalf("list = %#v, %v; want one fact", values, err)
	}
	generations, err := store.ListGenerations(ctx, factScope, "conversation")
	if err != nil || !reflect.DeepEqual(generations, []string{factGeneration}) {
		t.Fatalf("generations = %#v, %v", generations, err)
	}
	scopeFacts, err := store.ListScope(ctx, factScope)
	if err != nil || len(scopeFacts) != 1 || scopeFacts[0].ID != "a" {
		t.Fatalf("scope scan = %#v, %v; want one fact", scopeFacts, err)
	}
}

func TestFactStoreBackfillsAGenerationWrittenBeforeTheIndex(t *testing.T) {
	ctx := context.Background()
	store := newFactStore(t, newTestWorkspace(t))
	for _, request := range []AddRequest{factRequest("a", "first"), factRequest("b", "second")} {
		if _, err := store.Add(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	// A generation derived by a build without the identity area: the facts are
	// there, the identities are not.
	indexPrefix, err := store.hashIndexPrefix(factScope, "conversation", factGeneration)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := store.keysIn(ctx, indexPrefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if err := store.kv.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if complete, err := store.hashIndexComplete(ctx, factScope, "conversation", factGeneration); err != nil || complete {
		t.Fatalf("area reads complete without an index: %v, %v", complete, err)
	}

	// The first add to such a generation merges into the identity it already
	// holds instead of writing a second fact with the same text.
	request := factRequest("c", "first")
	request.Provenance = append(request.Provenance, corememory.SourceRef{
		Kind: corememory.SourceMessage, ID: "message-2", Revision: "1",
	})
	merged, err := store.Add(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if merged.ID != "a" || len(merged.Provenance) != 2 {
		t.Fatalf("merged = %#v", merged)
	}
	values, err := store.List(ctx, factScope, "conversation", ListOptions{})
	if err != nil || len(values) != 2 {
		t.Fatalf("list = %#v, %v; want the two existing facts", values, err)
	}
	// The scan recorded every fact it saw, not only the match, and closed the
	// area.
	if complete, err := store.hashIndexComplete(ctx, factScope, "conversation", factGeneration); err != nil || !complete {
		t.Fatalf("area complete after backfill = %v, %v", complete, err)
	}
	for id, text := range map[string]string{"a": "first", "b": "second"} {
		hash := CanonicalHash(NormalizeText(text))
		keys, err := store.indexedFactIDs(ctx, factScope, "conversation", factGeneration, hash)
		if err != nil || !reflect.DeepEqual(keys, []string{id}) {
			t.Fatalf("identity of %q = %v, %v; want [%s]", text, keys, err, id)
		}
	}
}

func TestFactStoreDropsAnIdentityWhoseFactIsGone(t *testing.T) {
	ctx := context.Background()
	store := newFactStore(t, newTestWorkspace(t))
	if _, err := store.Add(ctx, factRequest("a", "first")); err != nil {
		t.Fatal(err)
	}
	// An add interrupted between the identity write and the fact write leaves
	// an entry naming a fact that does not exist.
	orphanHash := CanonicalHash(NormalizeText("orphan"))
	if err := store.indexCanonicalHash(ctx, factScope, "conversation", factGeneration, orphanHash, "ghost"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Add(ctx, factRequest("b", "orphan"))
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != "b" {
		t.Fatalf("add of orphaned text = %q, want a new fact", stored.ID)
	}
	identities, err := store.indexedFactIDs(ctx, factScope, "conversation", factGeneration, orphanHash)
	if err != nil || !reflect.DeepEqual(identities, []string{"b"}) {
		t.Fatalf("identities = %v, %v; want the stale entry dropped", identities, err)
	}
}

func TestFactStoreRetireGenerationRetiresTheIdentityArea(t *testing.T) {
	ctx := context.Background()
	store := newFactStore(t, newTestWorkspace(t))
	if _, err := store.Add(ctx, factRequest("a", "first")); err != nil {
		t.Fatal(err)
	}
	other := factRequest("b", "second")
	other.Generation = "policy-b"
	if _, err := store.Add(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishActiveGeneration(ctx, factScope, "conversation", "policy-b"); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RetireGeneration(ctx, factScope, "conversation", factGeneration)
	if err != nil || removed != 1 {
		t.Fatalf("retire = %d, %v; want the one fact", removed, err)
	}
	prefix, err := store.hashIndexPrefix(factScope, "conversation", factGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if keys, err := store.keysIn(ctx, prefix); err != nil || len(keys) != 0 {
		t.Fatalf("retired identity area = %v, %v", keys, err)
	}
	// The identity of the surviving generation is untouched.
	keys, err := store.indexedFactIDs(ctx, factScope, "conversation", "policy-b", CanonicalHash(NormalizeText("second")))
	if err != nil || !reflect.DeepEqual(keys, []string{"b"}) {
		t.Fatalf("surviving identity = %v, %v", keys, err)
	}
}
