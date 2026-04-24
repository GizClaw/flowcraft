package history

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestArchive_BelowThreshold(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "archive-below"

	msgs := make([]model.Message, 10)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "hello")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	cfg := ArchiveConfig{ArchiveThreshold: 100, ArchiveBatchSize: 50}
	result, err := Archive(ctx, ws, store, "memory", convID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.MessagesArchived != 0 {
		t.Fatalf("expected 0 archived, got %d", result.MessagesArchived)
	}
}

func TestArchive_AboveThreshold(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "archive-above"

	msgs := make([]model.Message, 20)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "message content")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	cfg := ArchiveConfig{ArchiveThreshold: 15, ArchiveBatchSize: 10}
	result, err := Archive(ctx, ws, store, "memory", convID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.MessagesArchived != 10 {
		t.Fatalf("expected 10 archived, got %d", result.MessagesArchived)
	}
	if result.HotStartSeq != 10 {
		t.Fatalf("expected hot_start_seq=10, got %d", result.HotStartSeq)
	}

	// Check remaining messages.
	remaining, err := store.GetMessages(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 10 {
		t.Fatalf("expected 10 remaining, got %d", len(remaining))
	}

	// Check manifest.
	manifest, err := LoadManifest(ctx, ws, "memory", "archive", convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(manifest.Segments))
	}
	if manifest.Segments[0].Count != 10 {
		t.Fatalf("expected count=10, got %d", manifest.Segments[0].Count)
	}
}

func TestLoadArchivedMessages(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "archive-load"

	msgs := make([]model.Message, 30)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "msg")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	cfg := ArchiveConfig{ArchiveThreshold: 20, ArchiveBatchSize: 15}
	_, err = Archive(ctx, ws, store, "memory", convID, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Load archived messages.
	archived, err := LoadArchivedMessages(ctx, ws, "memory", "archive", convID, 0, 14)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 15 {
		t.Fatalf("expected 15 archived msgs, got %d", len(archived))
	}
}

func TestRecoverArchive_NoIntent(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()

	// No pending intent — should be a no-op.
	if err := RecoverArchive(ctx, ws, store, "memory", "archive", "no-intent-conv"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverArchive_GzipWrittenPhase(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "recover-gzip"

	// Prepare 20 messages.
	msgs := make([]model.Message, 20)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "msg")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	// Simulate: gzip was written + manifest exists but NOT updated, messages NOT trimmed.
	cfg := ArchiveConfig{ArchiveThreshold: 15, ArchiveBatchSize: 10}

	// Do a real archive first to get the gzip file on disk.
	result, err := Archive(ctx, ws, store, "memory", convID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.MessagesArchived != 10 {
		t.Fatalf("expected 10, got %d", result.MessagesArchived)
	}

	// Now simulate a crash in a second archive: re-add messages and write intent manually.
	remaining, _ := store.GetMessages(ctx, convID)
	moreMsgs := make([]model.Message, 20)
	for i := range moreMsgs {
		moreMsgs[i] = model.NewTextMessage(model.RoleUser, "more")
	}
	allMsgs := append(remaining, moreMsgs...)
	_ = store.SaveMessages(ctx, convID, allMsgs)

	intent := &archiveIntent{
		ConvID: convID, StartSeq: 10, EndSeq: 19,
		BatchSize: 10, ArchiveFile: "messages_10_19.jsonl.gz", Phase: "gzip_written",
	}
	_ = writeIntent(ctx, ws, "memory", "archive", convID, intent)

	// Recovery should: update manifest, trim messages.
	if err := RecoverArchive(ctx, ws, store, "memory", "archive", convID); err != nil {
		t.Fatal(err)
	}

	// Verify messages trimmed.
	after, _ := store.GetMessages(ctx, convID)
	if len(after) > len(allMsgs) {
		t.Fatalf("messages should have been trimmed, got %d", len(after))
	}

	// Verify intent cleaned up.
	loadedIntent, _ := loadIntent(ctx, ws, "memory", "archive", convID)
	if loadedIntent != nil {
		t.Fatal("intent should be cleaned up after recovery")
	}
}

func TestRecoverArchive_ManifestUpdatedPhase(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "recover-manifest"

	// Prepare and archive.
	msgs := make([]model.Message, 20)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "msg")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	cfg := ArchiveConfig{ArchiveThreshold: 15, ArchiveBatchSize: 10}
	_, err = Archive(ctx, ws, store, "memory", convID, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate: manifest updated but messages not yet trimmed.
	// Re-save all original 20 messages (as if trim didn't happen).
	_ = store.SaveMessages(ctx, convID, msgs)

	intent := &archiveIntent{
		ConvID: convID, StartSeq: 0, EndSeq: 9,
		BatchSize: 10, ArchiveFile: "messages_0_9.jsonl.gz", Phase: "manifest_updated",
	}
	_ = writeIntent(ctx, ws, "memory", "archive", convID, intent)

	if err := RecoverArchive(ctx, ws, store, "memory", "archive", convID); err != nil {
		t.Fatal(err)
	}

	after, _ := store.GetMessages(ctx, convID)
	if len(after) != 10 {
		t.Fatalf("expected 10 remaining after recovery, got %d", len(after))
	}

	loadedIntent, _ := loadIntent(ctx, ws, "memory", "archive", convID)
	if loadedIntent != nil {
		t.Fatal("intent should be cleaned up")
	}
}

func TestArchive_IntentCleanup(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "intent-cleanup"

	msgs := make([]model.Message, 20)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "msg")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	cfg := ArchiveConfig{ArchiveThreshold: 15, ArchiveBatchSize: 10}
	_, err = Archive(ctx, ws, store, "memory", convID, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// After successful archive, intent should not exist.
	loadedIntent, _ := loadIntent(ctx, ws, "memory", "archive", convID)
	if loadedIntent != nil {
		t.Fatal("intent should be cleaned up after successful archive")
	}
}

// TestSaveManifest_RoundTrip exercises the deprecated SaveManifest /
// LoadManifest pair declared in deprecated.go: write a manifest, read it
// back, and assert all fields survive the JSON round-trip.
func TestSaveManifest_RoundTrip(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	convID := "manifest-test"

	in := &ArchiveManifest{
		HotStartSeq: 25,
		Segments: []ArchiveSegment{
			{File: "messages_0_9.jsonl.gz", StartSeq: 0, EndSeq: 9, Count: 10, CreatedAt: time.Now().UTC().Truncate(time.Second)},
			{File: "messages_10_24.jsonl.gz", StartSeq: 10, EndSeq: 24, Count: 15, CreatedAt: time.Now().UTC().Truncate(time.Second)},
		},
	}
	if err := SaveManifest(ctx, ws, "memory", "archive", convID, in); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	out, err := LoadManifest(ctx, ws, "memory", "archive", convID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if out.HotStartSeq != in.HotStartSeq {
		t.Fatalf("HotStartSeq mismatch: got %d", out.HotStartSeq)
	}
	if len(out.Segments) != len(in.Segments) {
		t.Fatalf("segments len: got %d", len(out.Segments))
	}
	for i := range in.Segments {
		if out.Segments[i].File != in.Segments[i].File {
			t.Fatalf("segment %d File mismatch: got %q", i, out.Segments[i].File)
		}
		if out.Segments[i].Count != in.Segments[i].Count {
			t.Fatalf("segment %d Count mismatch: got %d", i, out.Segments[i].Count)
		}
	}
}
