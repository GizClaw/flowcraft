package maintain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/recall"
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

// writeLegacyOverlay plants the document shape a deployment before the stamp
// wrote: item identity to effect, with no policy attached. An upgrade finds it
// under the same key.
func writeLegacyOverlay(t *testing.T, kv storage.Store, scope corememory.Scope, identity string, factor float64) {
	t.Helper()
	key, err := overlayKey(scope)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]Overlay{
		identity: {Factor: factor, UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
}

// TestOverlayFromAnotherPolicyIsIgnoredAndReported pins the upgrade path of
// #604's overlay item: judgments an earlier version or tuning wrote are not
// applied to another policy's scores, and the condition is visible to a host
// that would schedule the pass which replaces them.
func TestOverlayFromAnotherPolicyIsIgnoredAndReported(t *testing.T) {
	ctx := context.Background()
	store, kv := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	identity, err := itemKey(scope, "conv-1", "stale")
	if err != nil {
		t.Fatal(err)
	}

	// The unstamped shape: no policy can be attributed to it.
	writeLegacyOverlay(t, kv, scope, identity, 0.3)
	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || !status.Stale || status.Entries != 1 || status.StoredProducer != "" {
		t.Fatalf("status of an unstamped overlay = %+v", status)
	}
	if status.ExpectedProducer == "" {
		t.Fatalf("status names no expected producer: %+v", status)
	}
	if overlay, err := store.Load(ctx, scope); err != nil || len(overlay) != 0 {
		t.Fatalf("ignored overlay = %#v, %v", overlay, err)
	}
	if scores, err := store.ScoreOverlay(ctx, scope); err != nil || len(scores) != 0 {
		t.Fatalf("scores from an ignored overlay = %#v, %v", scores, err)
	}
	if factor, err := store.Adjust(ctx, scope, identity); err != nil || factor != 1 {
		t.Fatalf("adjustment from an ignored overlay = %v, %v", factor, err)
	}

	// A stamped document from a different tuning is ignored the same way: the
	// stamp names the pass, not just the format.
	tuned := Config{SimilarityThreshold: 0.9}
	tunedStore, err := NewStore(kv, WithConfig(tuned))
	if err != nil {
		t.Fatal(err)
	}
	tunedPlan, err := DetectWithConfig(scope, map[string][]factview.Fact{}, time.Now().UTC(), tuned)
	if err != nil {
		t.Fatal(err)
	}
	tunedPlan.Decays = []Decay{{FactID: "stale", Conversation: "conv-1", Score: 0.4}}
	if err := tunedStore.Save(ctx, tunedPlan); err != nil {
		t.Fatal(err)
	}
	if status, err := store.Status(ctx, scope); err != nil ||
		!status.Stale || status.StoredProducer != tunedPlan.ProducerVersion {
		t.Fatalf("status of a foreign overlay = %+v, %v", status, err)
	}
	if scores, err := store.ScoreOverlay(ctx, scope); err != nil || len(scores) != 0 {
		t.Fatalf("scores from a foreign overlay = %#v, %v", scores, err)
	}
	// The deployment that wrote it reads it back, so the stamp is what
	// separates the two, not the document format.
	if scores, err := tunedStore.ScoreOverlay(ctx, scope); err != nil || len(scores) != 1 {
		t.Fatalf("scores at the writing deployment = %#v, %v", scores, err)
	}
}

// TestSaveRefusesAPlanFromAnotherPolicy pins the wiring guard: an overlay this
// store's own read path would ignore is a mistake to write, not a state to
// produce, so the save fails instead of publishing judgments nobody reads.
func TestSaveRefusesAPlanFromAnotherPolicy(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := store.Save(ctx, Plan{Scope: scope, ProducerVersion: "maintain-v1"}); err == nil ||
		!strings.Contains(err.Error(), "maintain-v1") {
		t.Fatalf("save of a foreign plan = %v", err)
	}
	// A plan that names no pass is written under this store's own, which is the
	// shape a host that plans its own overlay produces.
	if err := store.Save(ctx, Plan{
		Scope: scope, Decays: []Decay{{FactID: "stale", Conversation: "conv-1", Score: 0.2}},
	}); err != nil {
		t.Fatal(err)
	}
	if scores, err := store.ScoreOverlay(ctx, scope); err != nil || len(scores) != 1 {
		t.Fatalf("scores after an unnamed plan = %#v, %v", scores, err)
	}
}

// TestRunScopeClearsAnIgnoredOverlay is the acceptance half of the same item:
// a host reads the flag, runs the pass, and the pass replaces the judgments its
// policy could not read.
func TestRunScopeClearsAnIgnoredOverlay(t *testing.T) {
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
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	identity, err := itemKey(scope, "conv-1", "stale")
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyOverlay(t, kv, scope, identity, 0.3)

	service := &Service{Facts: facts, Store: store}
	plan, err := service.RunScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IgnoredOverlay {
		t.Fatalf("pass did not report the overlay it replaced: %+v", plan)
	}
	if plan.ProducerVersion == "" {
		t.Fatalf("pass reported no stamp: %+v", plan)
	}
	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || status.Stale || status.StoredProducer != plan.ProducerVersion {
		t.Fatalf("status after the pass = %+v", status)
	}
	// The flag is cleared once: a second pass replaces judgments its own policy
	// could read.
	again, err := service.RunScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if again.IgnoredOverlay {
		t.Fatalf("second pass reported an ignored overlay: %+v", again)
	}
}

// staticReceipts is a receipt source a test drives directly: the maintenance
// pass reads exactly what the read path is claimed to have returned.
type staticReceipts map[string]recall.Receipt

func (receipts staticReceipts) Receipts(context.Context, corememory.Scope) (map[string]recall.Receipt, error) {
	return receipts, nil
}

// TestRunScopeReinforcesRecalledFacts is the v3 acceptance fixture end to end
// through the service: a fact imported with an old event time is decayed, and
// the receipt of a later context read lifts it back out of the overlay --
// without the canonical fact changing.
func TestRunScopeReinforcesRecalledFacts(t *testing.T) {
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
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	facts, err := factview.NewFactStore(logStore, kv, factview.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(kv, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact-1", Generation: "generation-1", Scope: scope, ConversationID: "conv-1",
		Content:    coremessage.NewTextContent("Alice likes tea"),
		EventTime:  now.Add(-1250 * time.Hour),
		Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := facts.PublishActiveGeneration(ctx, scope, "conv-1", "generation-1"); err != nil {
		t.Fatal(err)
	}
	identity, err := itemKey(scope, "conv-1", "fact-1")
	if err != nil {
		t.Fatal(err)
	}

	// A pass without receipts is the imported history judgment: the fact is
	// 1250h old against a 720h half-life and is decayed to ~0.3.
	service := &Service{Facts: facts, Store: store, Clock: func() time.Time { return now }}
	plan, err := service.RunScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecalledFacts != 0 || len(plan.Decays) != 1 || plan.Decays[0].FactID != "fact-1" {
		t.Fatalf("plan before any recall = %#v", plan)
	}
	if plan.Decays[0].Score > 0.31 || plan.Decays[0].Score < 0.29 {
		t.Fatalf("imported fact score = %v", plan.Decays[0].Score)
	}
	scores, err := store.ScoreOverlay(ctx, scope)
	if err != nil || len(scores) != 1 {
		t.Fatalf("overlay after the import pass = %#v, %v", scores, err)
	}

	// The read path returned the fact an hour ago: the next pass judges it
	// from that recall, finds it fresh, and the overlay it writes is empty --
	// the call is served at its un-decayed score again.
	service.Recalls = staticReceipts{identity: {
		Identity: identity, Count: 3, Last: now.Add(-time.Hour), LastEvent: "run-1",
	}}
	plan, err = service.RunScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecalledFacts != 1 || len(plan.Decays) != 0 {
		t.Fatalf("plan after a recall = %#v", plan)
	}
	scores, err = store.ScoreOverlay(ctx, scope)
	if err != nil || len(scores) != 0 {
		t.Fatalf("overlay after a recall = %#v, %v", scores, err)
	}
}

// TestOverlayStatusReportsExpiry pins the D8 signal: a stored overlay this
// pass wrote but has not refreshed within one half-life reports Expired, so a
// host can see that the read path is serving factors measured against an old
// clock. An overlay from another pass is ignored rather than expired, and a
// scope with no overlay reports neither.
func TestOverlayStatusReportsExpiry(t *testing.T) {
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
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store, err := NewStore(kv, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}

	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if status.Stored || status.Stale || status.Expired || !status.UpdatedAt.IsZero() {
		t.Fatalf("status without an overlay = %+v", status)
	}

	if err := store.Save(ctx, Plan{
		Scope: scope, Decays: []Decay{{FactID: "stale", Conversation: "conv-1", Score: 0.3}},
	}); err != nil {
		t.Fatal(err)
	}
	status, err = store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || status.Stale || status.Expired || !status.UpdatedAt.Equal(now) {
		t.Fatalf("fresh status = %+v", status)
	}

	now = now.Add(720*time.Hour + time.Hour)
	status, err = store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Expired || status.Stale {
		t.Fatalf("status past the half-life = %+v", status)
	}

	foreign, err := NewStore(kv, WithClock(func() time.Time { return now }),
		WithConfig(Config{SimilarityThreshold: 0.9}))
	if err != nil {
		t.Fatal(err)
	}
	status, err = foreign.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stale || status.Expired {
		t.Fatalf("status at another pass = %+v", status)
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
