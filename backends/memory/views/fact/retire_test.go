package fact

import (
	"context"
	"testing"
	"time"
)

// A policy change leaves the replaced generation stored, which is what makes a
// rollback possible -- and what makes retirement necessary. These tests pin the
// sweep: the active generation is never retired, the records of a retired
// generation stop being readable, and the sweep is idempotent.
func TestFactStoreRetireGenerationKeepsTheActiveOne(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)
	store := newFactStore(t, newTestWorkspace(t), WithClock(func() time.Time { return now }))

	first := factRequest("fact-1", "first policy")
	first.Generation = "policy-a"
	if _, err := store.Add(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := factRequest("fact-2", "second policy")
	second.Generation = "policy-b"
	if _, err := store.Add(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishActiveGeneration(ctx, factScope, "conversation", "policy-b"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.RetireGeneration(ctx, factScope, "conversation", "policy-b"); err == nil {
		t.Fatal("the active generation was retired")
	}
	removed, err := store.RetireGeneration(ctx, factScope, "conversation", "policy-a")
	if err != nil || removed != 1 {
		t.Fatalf("retire = %d, %v", removed, err)
	}
	if values, err := store.List(ctx, factScope, "conversation", ListOptions{Generation: "policy-a"}); err != nil || len(values) != 0 {
		t.Fatalf("retired generation still readable: %#v, %v", values, err)
	}
	if values, err := store.List(ctx, factScope, "conversation", ListOptions{}); err != nil || len(values) != 1 {
		t.Fatalf("active generation = %#v, %v", values, err)
	}
	if generations, err := store.ListGenerations(ctx, factScope, "conversation"); err != nil || len(generations) != 1 || generations[0] != "policy-b" {
		t.Fatalf("generations = %#v, %v", generations, err)
	}
	// Retrying the sweep is the normal case: a retired generation is absent,
	// not an error.
	if removed, err = store.RetireGeneration(ctx, factScope, "conversation", "policy-a"); err != nil || removed != 0 {
		t.Fatalf("retry = %d, %v", removed, err)
	}
}

func TestFactStoreRetireGenerationsKeepsWhatTheCallerNames(t *testing.T) {
	ctx := context.Background()
	store := newFactStore(t, newTestWorkspace(t), WithClock(func() time.Time {
		return time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)
	}))
	for _, generation := range []string{"policy-a", "policy-b", "policy-c"} {
		request := factRequest("fact-"+generation, "text of "+generation)
		request.Generation = generation
		if _, err := store.Add(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PublishActiveGeneration(ctx, factScope, "conversation", "policy-c"); err != nil {
		t.Fatal(err)
	}
	// Keep one generation for rollback; the active one is kept implicitly.
	if removed, err := store.RetireGenerations(ctx, factScope, "conversation", "policy-b"); err != nil || removed != 1 {
		t.Fatalf("sweep = %d, %v", removed, err)
	}
	generations, err := store.ListGenerations(ctx, factScope, "conversation")
	if err != nil || len(generations) != 2 || generations[0] != "policy-b" || generations[1] != "policy-c" {
		t.Fatalf("generations = %#v, %v", generations, err)
	}
}
