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
