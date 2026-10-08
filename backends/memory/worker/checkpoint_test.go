package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func TestKVCheckpointsRoundTripAndPolicyIsolation(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store, err := NewKVCheckpoints(kv, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if _, ok, err := store.LoadWatermark(ctx, scope, "message", "conversation", "policy-a"); err != nil || ok {
		t.Fatalf("missing watermark = %v/%v, want false/nil", ok, err)
	}
	watermark := SourceWatermark{
		Scope: scope, StreamKind: "message", StreamID: "conversation",
		PolicyDigest: "policy-a", Cursor: 7,
	}
	if err := store.SaveWatermark(ctx, watermark); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := store.LoadWatermark(ctx, scope, "message", "conversation", "policy-a")
	if err != nil || !ok {
		t.Fatalf("load = %v/%v", ok, err)
	}
	if loaded.Cursor != 7 || !loaded.UpdatedAt.Equal(now) {
		t.Fatalf("loaded = %#v", loaded)
	}
	if _, ok, err := store.LoadWatermark(ctx, scope, "message", "conversation", "policy-b"); err != nil || ok {
		t.Fatalf("policy isolation = %v/%v, want false/nil", ok, err)
	}
	invalid := watermark
	invalid.Cursor = 0
	if err := store.SaveWatermark(ctx, invalid); err == nil {
		t.Fatal("zero cursor accepted")
	}
	// A persisted watermark whose payload does not match its key must be
	// rejected instead of silently resuming at the wrong cursor.
	key, err := watermarkKey(scope, "message", "conversation", "policy-a")
	if err != nil {
		t.Fatal(err)
	}
	mismatched := watermark
	mismatched.StreamID = "other"
	data, err := json.Marshal(mismatched)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadWatermark(ctx, scope, "message", "conversation", "policy-a"); err == nil {
		t.Fatal("mismatched watermark accepted")
	}
}

// TestKVCheckpointsRetireWatermarksDropsOnePolicyOfOneStream pins the cursor half
// of retiring a generation: only the named policy of the named stream loses its
// progress, the count describes what was stored, and a retry finds nothing left
// to drop -- so a sweep that failed halfway is simply run again.
func TestKVCheckpointsRetireWatermarksDropsOnePolicyOfOneStream(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewKVCheckpoints(kv)
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	watermarks := []SourceWatermark{
		{Scope: scope, StreamKind: "message", StreamID: "conversation", PolicyDigest: "policy-a", Cursor: 7},
		{Scope: scope, StreamKind: "message", StreamID: "conversation", PolicyDigest: "policy-b", Cursor: 3},
		{Scope: scope, StreamKind: "message", StreamID: "other", PolicyDigest: "policy-b", Cursor: 4},
		{Scope: scope, StreamKind: "document-events", StreamID: "scope", PolicyDigest: "policy-b", Cursor: 5},
	}
	for _, watermark := range watermarks {
		if err := store.SaveWatermark(ctx, watermark); err != nil {
			t.Fatal(err)
		}
	}

	retired, err := store.RetireWatermarks(ctx, scope, "message", "conversation", []string{"policy-b"})
	if err != nil || retired != 1 {
		t.Fatalf("retired = %d, %v, want 1", retired, err)
	}
	if _, ok, err := store.LoadWatermark(ctx, scope, "message", "conversation", "policy-b"); err != nil || ok {
		t.Fatalf("the retired cursor is still stored = %v/%v", ok, err)
	}
	for _, watermark := range []SourceWatermark{watermarks[0], watermarks[2], watermarks[3]} {
		loaded, ok, err := store.LoadWatermark(ctx, watermark.Scope, watermark.StreamKind, watermark.StreamID, watermark.PolicyDigest)
		if err != nil || !ok || loaded.Cursor != watermark.Cursor {
			t.Fatalf("cursor of %s/%s/%s = %#v, %v, %v", watermark.StreamKind, watermark.StreamID, watermark.PolicyDigest, loaded, ok, err)
		}
	}

	// Retiring is idempotent, and a cursor that was never stored is not a
	// failure: the sweep can be retried.
	if retired, err := store.RetireWatermarks(ctx, scope, "message", "conversation", []string{"policy-b", "policy-b", ""}); err != nil || retired != 0 {
		t.Fatalf("retry = %d, %v, want 0", retired, err)
	}
	if retired, err := store.RetireWatermarks(ctx, scope, "message", "conversation", []string{"policy-a", "policy-absent"}); err != nil || retired != 1 {
		t.Fatalf("retired = %d, %v, want the stored policy only", retired, err)
	}
	if _, err := store.RetireWatermarks(ctx, scope, "", "conversation", []string{"policy-a"}); err == nil {
		t.Fatal("an unnamed stream was accepted")
	}
}
