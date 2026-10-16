package memory

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/recall"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// TestRecallReceiptsFeedMaintenance is the O6 acceptance fixture at the
// assembly surface: commit and derive are an import and produce no receipt,
// the imported fact is decayed by the pass that runs before any read, and the
// receipt of a recall is what lifts it back out of the overlay. The fact's
// score rises back to its pre-maintenance value because reinforcement
// compensates decay instead of adding to it.
func TestRecallReceiptsFeedMaintenance(t *testing.T) {
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "interval": "0"
	}`
	assembly, ws := newTestAssemblyWith(t, settings,
		WithDeriver(fakeDeriver{eventTime: testNow.Add(-1250 * time.Hour)}))
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	factScore := func(recallEventID string) float64 {
		t.Helper()
		result, err := assembly.Context(ctx, corememory.ContextRequest{
			Scope: testScope(), ConversationID: "conv-1", Query: "tea",
			RecallEventID: recallEventID,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range result.Items {
			if item.Kind == corememory.ContextFact {
				return item.Score
			}
		}
		t.Fatalf("fact missing from context: %#v", result.Items)
		return 0
	}

	baseline := factScore("")
	// The import wrote no receipt, so the first pass is the pure event-time
	// judgment: 1250h of a 720h half-life decays the fact to ~0.3.
	plan, err := assembly.Maintain(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecalledFacts != 0 || len(plan.Decays) != 1 || plan.Decays[0].FactID != "fact-alice-tea" {
		t.Fatalf("plan before any recall = %#v", plan)
	}
	decayed := factScore("")
	if decayed > baseline*0.35 {
		t.Fatalf("decayed score = %v, baseline = %v", decayed, baseline)
	}

	// A recall with an event id is a receipt in the shared store.
	if score := factScore("invocation-1"); score <= 0 {
		t.Fatalf("recall returned score %v", score)
	}
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := recall.NewStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	identity := corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: "conv-1", ItemID: "fact-alice-tea",
	}.Identity(testScope())
	index, err := receipts.Receipts(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(index) != 1 || index[identity].Count != 1 || !index[identity].Last.Equal(testNow) {
		t.Fatalf("receipts after the recall = %#v", index)
	}

	// The next pass reads the receipt, judges the fact from its last recall,
	// and finds it fresh: the overlay it writes is empty, so the read path
	// serves the un-decayed score again.
	plan, err = assembly.Maintain(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecalledFacts != 1 || len(plan.Decays) != 0 {
		t.Fatalf("plan after a recall = %#v", plan)
	}
	recovered := factScore("")
	if math.Abs(recovered-baseline) > 1e-9 {
		t.Fatalf("recalled fact score = %v, want the baseline %v", recovered, baseline)
	}
}

// TestMaintainDiagnosticsReportOverlayExpiry pins the D8 read at the assembly
// surface: a host sees in Diagnostics that the overlay it is serving was
// written too long ago, which is its signal to schedule the pass.
func TestMaintainDiagnosticsReportOverlayExpiry(t *testing.T) {
	now := testNow
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "interval": "0"
	}`
	assembly, _ := newTestAssemblyWith(t, settings,
		WithClock(func() time.Time { return now }),
		WithDeriver(fakeDeriver{eventTime: testNow.Add(-1250 * time.Hour)}))
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := assembly.Maintain(ctx, testScope()); err != nil {
		t.Fatal(err)
	}
	status, err := assembly.OverlayStatus(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || status.Stale || status.Expired || !status.UpdatedAt.Equal(testNow) {
		t.Fatalf("fresh overlay status = %+v", status)
	}

	now = testNow.Add(720*time.Hour + time.Minute)
	diagnostics, err := assembly.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.Scopes) != 1 || diagnostics.Scopes[0].Overlay == nil {
		t.Fatalf("diagnostics = %+v", diagnostics.Scopes)
	}
	overlay := diagnostics.Scopes[0].Overlay
	if !overlay.Expired || overlay.Stale {
		t.Fatalf("diagnostics overlay = %+v", overlay)
	}
}
