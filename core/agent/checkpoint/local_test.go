package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

func writeCheckpoint(t *testing.T, store *Local, id string) agent.Checkpoint {
	t.Helper()
	cp := agent.Checkpoint{Schema: agent.CheckpointSchema, Runtime: "sha256:x"}
	cp.Task.ID = id
	if err := store.Write(context.Background(), cp); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return cp
}

// Appending twice must leave the newest checkpoint readable and the log intact.
func TestWriteAppendsAndLatestReadsTheNewest(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeCheckpoint(t, store, "first")
	writeCheckpoint(t, store, "second")

	latest, err := store.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.Task.ID != "second" {
		t.Errorf("latest = %q, want second", latest.Task.ID)
	}
}

func TestLatestWithoutAWrite(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.Latest(context.Background()); !errors.Is(err, agent.ErrNoCheckpoint) {
		t.Fatalf("err = %v, want ErrNoCheckpoint", err)
	}
}

// The same bytes must yield the same reference, so repeated tool output costs a
// stat rather than a write.
func TestPutBlobDeduplicates(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	first, err := store.PutBlob(ctx, []byte("hello"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	second, err := store.PutBlob(ctx, []byte("hello"))
	if err != nil {
		t.Fatalf("PutBlob again: %v", err)
	}
	if first != second {
		t.Errorf("refs differ: %s != %s", first, second)
	}
	body, err := store.GetBlob(ctx, first)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("blob = %q", body)
	}
}

func TestGetBlobRejectsAMalformedReference(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.GetBlob(context.Background(), "not-a-hash"); err == nil {
		t.Fatal("malformed reference accepted")
	}
}

func TestNewRequiresADirectory(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("empty directory accepted")
	}
}
