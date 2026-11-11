package summary

import (
	"context"
	"reflect"
	"testing"
)

// A generation's manifest bookmark is what makes a rollback possible and what a
// retention policy retires. These tests pin the sweep: the active generation
// cannot be retired, a retired generation stops being publishable, the immutable
// records stay so the kept generations keep reading, and the sweep is retryable.
func TestRetireGenerationsDropsTheBookmarkNotTheRecords(t *testing.T) {
	ctx := context.Background()
	store := newSummaryStore(t, newTestWorkspace(t))
	publishSummaryRecords(t, store, "generation-1", []Record{
		addSummaryRecord(t, store, "leaf-1", "generation-1", 1),
		addSummaryRecord(t, store, "leaf-2", "generation-1", 2),
	})
	publishSummaryRecords(t, store, "generation-2", []Record{
		addSummaryRecord(t, store, "leaf-3", "generation-2", 3),
	})

	generations, err := store.ListGenerations(ctx, summaryScope, "conversation")
	if err != nil || !reflect.DeepEqual(generations, []string{"generation-1", "generation-2"}) {
		t.Fatalf("generations = %#v, %v", generations, err)
	}
	if removed, err := store.RetireGeneration(ctx, summaryScope, "conversation", "generation-2"); err == nil || removed != 0 {
		t.Fatalf("the active generation was retired: %d, %v", removed, err)
	}
	if _, err := store.RetireGeneration(ctx, summaryScope, "conversation", ""); err == nil {
		t.Fatal("an empty generation was retired")
	}
	// Keeping generation-1 is the caller's retention decision; generation-2 is
	// kept because it is what the branch serves.
	if removed, err := store.RetireGenerations(ctx, summaryScope, "conversation", "generation-1"); err != nil || removed != 0 {
		t.Fatalf("sweep keeping both = %d, %v", removed, err)
	}
	if removed, err := store.RetireGenerations(ctx, summaryScope, "conversation"); err != nil || removed != 1 {
		t.Fatalf("sweep = %d, %v", removed, err)
	}
	generations, err = store.ListGenerations(ctx, summaryScope, "conversation")
	if err != nil || !reflect.DeepEqual(generations, []string{"generation-2"}) {
		t.Fatalf("generations after the sweep = %#v, %v", generations, err)
	}

	// A retired generation has nothing left to serve: the branch stays where it
	// is instead of moving onto a manifest whose facts a sweep took away.
	if manifest, found, err := store.PublishGeneration(ctx, summaryScope, "conversation", "generation-1"); err != nil || found {
		t.Fatalf("publish of a retired generation = %#v, %v, %v", manifest, found, err)
	}
	active, found, err := store.LoadActive(ctx, summaryScope, "conversation")
	if err != nil || !found || active.GenerationID != "generation-2" {
		t.Fatalf("active manifest = %#v, %v, %v", active, found, err)
	}

	// Retirement is unreachability, not erasure: the records of the retired
	// generation stay stored, and the kept generation still reads its own.
	records, err := store.List(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || !reflect.DeepEqual(recordIDs(records), []string{"leaf-1", "leaf-2", "leaf-3"}) {
		t.Fatalf("stored records = %#v, %v", recordIDs(records), err)
	}
	records, err = store.ListActive(ctx, summaryScope, "conversation", ListOptions{})
	if err != nil || !reflect.DeepEqual(recordIDs(records), []string{"leaf-3"}) {
		t.Fatalf("active records = %#v, %v", recordIDs(records), err)
	}
	if removed, err := store.RetireGenerations(ctx, summaryScope, "conversation"); err != nil || removed != 0 {
		t.Fatalf("retry = %d, %v", removed, err)
	}
	if removed, err := store.RetireGeneration(ctx, summaryScope, "conversation", "generation-absent"); err != nil || removed != 0 {
		t.Fatalf("retire of an absent generation = %d, %v", removed, err)
	}
}
