package summary

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestPublishGenerationRestoresTheManifestItPublished pins what a rollback
// reads: a generation is published beside the one it replaces, so serving it
// again is moving the branch back to the manifest that generation published --
// the same records, frontier and publication time -- even after the generation
// that replaced it published on top of it.
func TestPublishGenerationRestoresTheManifestItPublished(t *testing.T) {
	ctx := context.Background()
	ws := newTestWorkspace(t)
	now := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	store := newSummaryStore(t, ws, WithClock(func() time.Time { return now }))

	first := []Record{
		addSummaryRecord(t, store, "leaf-1", "generation-1", 1),
		addSummaryRecord(t, store, "leaf-2", "generation-1", 2),
	}
	firstManifest := publishSummaryRecords(t, store, "generation-1", first)
	second := []Record{
		addSummaryRecord(t, store, "leaf-3", "generation-2", 1),
	}
	secondManifest := publishSummaryRecords(t, store, "generation-2", second)
	if secondManifest.GenerationID != "generation-2" ||
		!reflect.DeepEqual(secondManifest.RecordIDs, []string{"leaf-3"}) {
		t.Fatalf("published manifest = %#v", secondManifest)
	}
	// The replaced generation is stored but its records are not what the
	// branch serves: nothing resolves to a manifest labelled for it.
	records, err := store.ListActive(ctx, summaryScope, "conversation", ListOptions{GenerationID: "generation-1"})
	if err != nil || len(records) != 0 {
		t.Fatalf("records of the replaced generation = %#v, %v", records, err)
	}

	restored, found, err := store.PublishGeneration(ctx, summaryScope, "conversation", "generation-1")
	if err != nil || !found {
		t.Fatalf("restored = %#v, %v, %v", restored, found, err)
	}
	if !reflect.DeepEqual(restored, firstManifest) {
		t.Fatalf("restored manifest = %#v, want %#v", restored, firstManifest)
	}
	active, found, err := store.LoadActive(ctx, summaryScope, "conversation")
	if err != nil || !found || !reflect.DeepEqual(active, firstManifest) {
		t.Fatalf("active manifest = %#v, %v, %v", active, found, err)
	}
	records, err = store.ListActive(ctx, summaryScope, "conversation", ListOptions{GenerationID: "generation-1"})
	if err != nil || !reflect.DeepEqual(recordIDs(records), []string{"leaf-1", "leaf-2"}) {
		t.Fatalf("records of the restored generation = %#v, %v", recordIDs(records), err)
	}

	// Reopening the store serves the generation the branch was moved back to:
	// the bookmark is stored, not remembered.
	reopened := newSummaryStore(t, ws)
	if active, found, err := reopened.LoadActive(ctx, summaryScope, "conversation"); err != nil || !found ||
		!reflect.DeepEqual(active, firstManifest) {
		t.Fatalf("reopened active manifest = %#v, %v, %v", active, found, err)
	}

	// The generation that is current again publishes on from its own point:
	// its new manifest replaces the restored one without reaching into the
	// chain of the generation it replaced.
	extended := append([]Record(nil), first...)
	extended = append(extended, addSummaryRecord(t, store, "leaf-4", "generation-1", 3))
	grown := publishSummaryRecords(t, store, "generation-1", extended)
	if !reflect.DeepEqual(grown.RecordIDs, []string{"leaf-1", "leaf-2", "leaf-4"}) {
		t.Fatalf("grown manifest = %#v", grown)
	}
	if restored, found, err := store.PublishGeneration(
		ctx, summaryScope, "conversation", "generation-2"); err != nil || !found ||
		!reflect.DeepEqual(restored, secondManifest) {
		t.Fatalf("manifest of the replaced generation = %#v, %v, %v", restored, found, err)
	}
}

func TestPublishGenerationLeavesTheBranchForAGenerationThatNeverPublished(t *testing.T) {
	ctx := context.Background()
	store := newSummaryStore(t, newTestWorkspace(t))
	manifest := publishSummaryRecords(t, store, "generation-1",
		[]Record{addSummaryRecord(t, store, "leaf-1", "generation-1", 1)})

	if _, found, err := store.PublishGeneration(ctx, summaryScope, "conversation", "generation-absent"); err != nil || found {
		t.Fatalf("found = %v, err = %v", found, err)
	}
	active, found, err := store.LoadActive(ctx, summaryScope, "conversation")
	if err != nil || !found || !reflect.DeepEqual(active, manifest) {
		t.Fatalf("active manifest = %#v, %v, %v", active, found, err)
	}
	if _, _, err := store.PublishGeneration(ctx, summaryScope, "conversation", ""); err == nil {
		t.Fatal("an empty generation was published")
	}
}

func addSummaryRecord(t *testing.T, store *SummaryStore, id, generation string, seq uint64) Record {
	t.Helper()
	request := summaryRequest(id)
	request.Level = L0
	request.GenerationID = generation
	request.CoverageRange = CoverageRange{StartSeq: seq, EndSeq: seq}
	request.SourceDigest = "source-" + id
	request.TransformSignature = "summary-v1-" + generation
	record, err := store.Add(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// publishSummaryRecords publishes the manifest of one generation's records and
// returns the manifest that was published, publication time included.
func publishSummaryRecords(t *testing.T, store *SummaryStore, generation string, records []Record) Manifest {
	t.Helper()
	manifest, err := BuildManifest(summaryScope, "conversation", generation, records)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishActive(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	active, found, err := store.LoadActive(context.Background(), summaryScope, "conversation")
	if err != nil || !found {
		t.Fatalf("active manifest = %v, %v", found, err)
	}
	return active
}
