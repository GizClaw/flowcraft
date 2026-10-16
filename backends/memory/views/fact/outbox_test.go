package fact

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/errdefs"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// failingSink stands in for a host whose outbox is down: the fact write
// itself still has to be durable.
type failingSink struct{}

func (failingSink) PublishFact(context.Context, Publication) error {
	return errors.New("outbox is down")
}

func newFactStoreWithOutbox(t *testing.T, ws workspace.Workspace, options ...Option) (*FactStore, *Outbox, storage.Log) {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := NewOutbox(logStore)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFactStore(logStore, kvStore, append(options, WithPublicationSink(outbox))...)
	if err != nil {
		t.Fatal(err)
	}
	return store, outbox, logStore
}

// TestOutboxHoldsEveryPublicationAndMatchesThePollingPath pins the two halves
// of O7 against each other: what the sink appends and what a consumer
// reconciling by polling (ListPublications) finds have to be the same set of
// publications, or a consumer would derive one set of conversations and miss
// another depending on how it was wired.
func TestOutboxHoldsEveryPublicationAndMatchesThePollingPath(t *testing.T) {
	ctx := context.Background()
	store, outbox, _ := newFactStoreWithOutbox(t, newTestWorkspace(t))
	if _, err := store.Add(ctx, factRequest("b", "second")); err != nil {
		t.Fatal(err)
	}
	// The same identity with provenance the stored fact does not have yet
	// merges into it rather than republishing it.
	merged := factRequest("b", "second")
	merged.Provenance = append(merged.Provenance,
		corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message-2", Revision: "2"})
	if _, err := store.Add(ctx, merged); err != nil {
		t.Fatal(err)
	}

	records, cursor, err := outbox.Read(ctx, factScope, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("outbox records = %#v, want the publish and the merge", records)
	}
	if records[0].Event != "published" || records[1].Event != "merged" {
		t.Fatalf("outbox events = %q, %q", records[0].Event, records[1].Event)
	}
	if cursor != 2 {
		t.Fatalf("cursor = %d, want the last sequence", cursor)
	}
	for _, record := range records {
		if record.ConversationID != "conversation" || record.FactID != "b" || record.Scope() != factScope {
			t.Fatalf("record = %#v", record)
		}
		if record.SchemaVersion != schemaVersion {
			t.Fatalf("record schema version = %d, want %d", record.SchemaVersion, schemaVersion)
		}
	}

	polled, err := store.ListPublications(ctx, factScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(polled) != len(records) {
		t.Fatalf("polling path found %d publications, outbox holds %d", len(polled), len(records))
	}
	// The polling path sorts by identity, the outbox is in stream order: the
	// two are compared as sets.
	sort.Slice(records, func(i, j int) bool { return records[i].PublicationID < records[j].PublicationID })
	for index, publication := range polled {
		if publication.PublicationID != records[index].PublicationID {
			t.Errorf("polling publication %d = %s, outbox record = %s",
				index, publication.PublicationID, records[index].PublicationID)
		}
		if publication.Event != records[index].Event || publication.RevisionDigest != records[index].RevisionDigest {
			t.Errorf("polling publication %d = %#v, outbox record = %#v", index, publication, records[index])
		}
	}
}

// TestOutboxReplayIsIdempotent replays the stream the way a consumer that
// lost its cursor would: every record is published again, and the stream is
// unchanged afterwards.
func TestOutboxReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, outbox, _ := newFactStoreWithOutbox(t, newTestWorkspace(t))
	if _, err := store.Add(ctx, factRequest("b", "second")); err != nil {
		t.Fatal(err)
	}
	records, cursor, err := outbox.Read(ctx, factScope, 0, 0)
	if err != nil || len(records) == 0 {
		t.Fatalf("records = %#v, %v", records, err)
	}
	for _, record := range records {
		publication := Publication{
			Fact:  Fact{ID: record.FactID, Scope: record.Scope(), ConversationID: record.ConversationID},
			Event: record.Event, RevisionDigest: record.RevisionDigest,
			PublicationID: record.PublicationID,
		}
		if err := outbox.PublishFact(ctx, publication); err != nil {
			t.Fatalf("replay of %s: %v", record.PublicationID, err)
		}
	}
	replayed, replayedCursor, err := outbox.Read(ctx, factScope, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, records) || replayedCursor != cursor {
		t.Fatalf("replay changed the stream:\n before %#v (%d)\n after  %#v (%d)",
			records, cursor, replayed, replayedCursor)
	}
}

// TestOutboxRejectsAPublicationIDCollision pins what an idempotency key can
// and cannot absorb: a retry of the same publication is swallowed, a
// different record under the same id is not.
func TestOutboxRejectsAPublicationIDCollision(t *testing.T) {
	ctx := context.Background()
	store, outbox, logStore := newFactStoreWithOutbox(t, newTestWorkspace(t))
	if _, err := store.Add(ctx, factRequest("b", "second")); err != nil {
		t.Fatal(err)
	}
	records, _, err := outbox.Read(ctx, factScope, 0, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, %v", records, err)
	}
	other := records[0]
	other.RevisionDigest = "a different revision"
	if err := outbox.PublishFact(ctx, Publication{
		Fact:  Fact{ID: other.FactID, Scope: factScope, ConversationID: other.ConversationID},
		Event: other.Event, RevisionDigest: other.RevisionDigest, PublicationID: other.PublicationID,
	}); !errdefs.IsConflict(err) {
		t.Fatalf("collision error = %v, want a conflict", err)
	}

	// A record whose key names a publication it does not carry is refused
	// too: it would hide the record that key belongs to.
	stream, err := publicationsStream(factScope)
	if err != nil {
		t.Fatal(err)
	}
	stray := other
	stray.FactID = "a"
	payload, err := json.Marshal(stray)
	if err != nil {
		t.Fatal(err)
	}
	const strayKey = "publication-appended-under-another-key"
	if _, err := logStore.Append(ctx, stream, []storage.Event{{
		Stream: stream, Type: publicationEventType, Payload: payload,
	}}, storage.AppendOptions{IdempotencyKey: strayKey}); err != nil {
		t.Fatal(err)
	}
	if err := outbox.PublishFact(ctx, Publication{
		Fact:  Fact{ID: other.FactID, Scope: factScope, ConversationID: other.ConversationID},
		Event: other.Event, RevisionDigest: other.RevisionDigest, PublicationID: strayKey,
	}); !errdefs.IsConflict(err) {
		t.Fatalf("second collision error = %v, want a conflict", err)
	}
}

// TestRetriedAddReEmitsTheSamePublication is the contract the sink's doc
// comment states: a sink that fails leaves the fact durable, and the retry
// re-emits the same logical event -- which lands in the outbox once.
func TestRetriedAddReEmitsTheSamePublication(t *testing.T) {
	ctx := context.Background()
	ws := newTestWorkspace(t)
	broken := newFactStore(t, ws, WithPublicationSink(failingSink{}))
	if _, err := broken.Add(ctx, factRequest("b", "second")); err == nil {
		t.Fatal("add with a failing sink returned no error")
	}
	store, outbox, _ := newFactStoreWithOutbox(t, ws)
	if _, err := store.Add(ctx, factRequest("b", "second")); err != nil {
		t.Fatal(err)
	}
	records, _, err := outbox.Read(ctx, factScope, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Event != "published" {
		t.Fatalf("records = %#v, want the one publication the retry emitted", records)
	}
}

// TestOutboxReadUsesTheStreamAsACursor pins the read side: records come back
// in stream order, and a consumer resuming after the last sequence reads what
// was appended since.
func TestOutboxReadUsesTheStreamAsACursor(t *testing.T) {
	ctx := context.Background()
	store, outbox, _ := newFactStoreWithOutbox(t, newTestWorkspace(t))
	if _, err := store.Add(ctx, factRequest("a", "first")); err != nil {
		t.Fatal(err)
	}
	first, cursor, err := outbox.Read(ctx, factScope, 0, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	if _, err := store.Add(ctx, factRequest("b", "second")); err != nil {
		t.Fatal(err)
	}
	rest, _, err := outbox.Read(ctx, factScope, cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].FactID != "b" {
		t.Fatalf("resumed page = %#v", rest)
	}
	empty, emptyCursor, err := outbox.Read(ctx, factScope, cursor+1, 0)
	if err != nil || len(empty) != 0 || emptyCursor != cursor+1 {
		t.Fatalf("read past the end = %#v, %d, %v", empty, emptyCursor, err)
	}
}
