package maintain

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func testStore(t *testing.T) (*Store, storage.Store) {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store, err := NewStore(kv, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	return store, kv
}

func TestStoreRoundTripAndScoreOverlay(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	plan := Plan{
		Scope: scope, AlgorithmVersion: AlgorithmVersion,
		Supersedes: []Supersede{{FactID: "old", Conversation: "conv-1", SupersededBy: "new", Similarity: 0.6}},
		Decays:     []Decay{{FactID: "stale", Conversation: "conv-1", Score: 0.3}},
	}
	if err := store.Save(ctx, plan); err != nil {
		t.Fatal(err)
	}
	overlay, err := store.Load(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay) != 2 {
		t.Fatalf("overlay = %#v", overlay)
	}
	scores, err := store.ScoreOverlay(ctx, scope)
	if err != nil || len(scores) != 2 {
		t.Fatalf("scores = %#v, %v", scores, err)
	}
	for _, factor := range scores {
		if factor != 0.5 && factor != 0.3 {
			t.Fatalf("unexpected factor %v", factor)
		}
	}
	// Saving an empty plan clears the previous effects.
	if err := store.Save(ctx, Plan{Scope: scope}); err != nil {
		t.Fatal(err)
	}
	overlay, err = store.Load(ctx, scope)
	if err != nil || len(overlay) != 0 {
		t.Fatalf("cleared overlay = %#v, %v", overlay, err)
	}
	if scores, err := store.ScoreOverlay(ctx, scope); err != nil || len(scores) != 0 {
		t.Fatalf("cleared scores = %#v, %v", scores, err)
	}
	// A missing overlay loads as empty rather than erroring.
	other := corememory.Scope{RuntimeID: "runtime", UserID: "other"}
	if scores, err := store.ScoreOverlay(ctx, other); err != nil || len(scores) != 0 {
		t.Fatalf("missing overlay = %#v, %v", scores, err)
	}
}

func TestStoreAdjustSingleIdentity(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime"}
	plan := Plan{Scope: scope, Decays: []Decay{{FactID: "stale", Conversation: "conv-1", Score: 0.25}}}
	if err := store.Save(ctx, plan); err != nil {
		t.Fatal(err)
	}
	key, err := itemKey(scope, "conv-1", "stale")
	if err != nil {
		t.Fatal(err)
	}
	factor, err := store.Adjust(ctx, scope, key)
	if err != nil || factor != 0.25 {
		t.Fatalf("adjust = %v, %v", factor, err)
	}
	missing, err := store.Adjust(ctx, scope, "context-item-missing")
	if err != nil || missing != 1 {
		t.Fatalf("missing adjust = %v, %v", missing, err)
	}
}
