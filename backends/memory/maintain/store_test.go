package maintain

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	docview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
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

// TestRunScopeReclaimsSupersededDocumentBuilds pins the reclaim half of a
// maintenance pass: the overlay is what the pass detects, and the derived
// document builds it no longer serves are what it removes. A service without a
// document view still runs, and reclaims nothing.
func TestRunScopeReclaimsSupersededDocumentBuilds(t *testing.T) {
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
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	docViews, err := docview.NewDocumentViewStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	provenance := []corememory.SourceRef{{Kind: corememory.SourceDocument, ID: "document"}}
	publish := func(version uint64) {
		t.Helper()
		chunk := docview.Chunk{
			ID: fmt.Sprintf("chunk-%d", version), Scope: scope,
			DatasetID: "dataset", DocumentID: "document", DocumentVersion: version,
			Ordinal: 0, Content: coremessage.Content{Parts: []coremessage.Part{
				coremessage.TextPart{Text: fmt.Sprintf("body %d", version)},
			}},
			Provenance: provenance,
		}
		if _, err := docViews.ReplaceDocument(ctx, docview.ReplaceRequest{
			Scope: scope, DatasetID: "dataset", DocumentID: "document",
			DocumentVersion: version, Chunks: []docview.Chunk{chunk},
		}); err != nil {
			t.Fatal(err)
		}
	}
	publish(1)
	publish(2)
	service := &Service{Facts: facts, Store: store, Documents: docViews}
	plan, err := service.RunScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ReclaimedChunks != 1 {
		t.Fatalf("reclaimed = %d, want the superseded build's chunk", plan.ReclaimedChunks)
	}
	if builds, err := docViews.ListBuilds(ctx, scope, "dataset", "document"); err != nil || len(builds) != 1 {
		t.Fatalf("builds after the pass = %#v, %v, want only the active one", builds, err)
	}
	// A pass that reclaims nothing reports nothing, and a deployment that
	// configures no document view still runs.
	again, err := service.RunScope(ctx, scope)
	if err != nil || again.ReclaimedChunks != 0 {
		t.Fatalf("second pass reclaimed %d, %v, want 0", again.ReclaimedChunks, err)
	}
	publish(3)
	service.Documents = nil
	plan, err = service.RunScope(ctx, scope)
	if err != nil || plan.ReclaimedChunks != 0 {
		t.Fatalf("pass without a document view reclaimed %d, %v, want 0", plan.ReclaimedChunks, err)
	}
	if builds, err := docViews.ListBuilds(ctx, scope, "dataset", "document"); err != nil || len(builds) != 2 {
		t.Fatalf("builds after a pass without a view = %#v, %v, want both", builds, err)
	}
}
