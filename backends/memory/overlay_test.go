package memory

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// legacyOverlayRoot mirrors the key a deployment before the overlay stamp wrote
// to. It is duplicated here on purpose: the fixture has to plant what an upgrade
// finds in a workspace, and calling the store's own writer would only match
// today's format against itself.
const legacyOverlayRoot = "maintain/v1/overlay"

func seedLegacyOverlay(
	t *testing.T,
	ws workspace.Workspace,
	scope corememory.Scope,
	identity string,
	factor float64,
) {
	t.Helper()
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]maintain.Overlay{identity: {Factor: factor}})
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(context.Background(), legacyOverlayRoot+"/"+partition, data); err != nil {
		t.Fatal(err)
	}
}

// TestOverlayStatusReportsAnIgnoredOverlay is the overlay acceptance criteria of
// #604 at the assembly surface: judgments an earlier producer wrote are reported
// as ignored rather than applied, the assembly-wide snapshot carries the same
// condition, and the next maintenance pass clears it.
func TestOverlayStatusReportsAnIgnoredOverlay(t *testing.T) {
	ctx := context.Background()
	assembly, ws := newTestAssemblyWith(t, testSettings)
	scope := testScope()
	identity := corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: "conv-1", ItemID: "fact-1",
	}.Identity(scope)
	seedLegacyOverlay(t, ws, scope, identity, 0.3)

	status, err := assembly.OverlayStatus(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || !status.Stale || status.Entries != 1 || status.ExpectedProducer == "" {
		t.Fatalf("overlay status = %+v", status)
	}
	diagnostics, err := assembly.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.Scopes) != 1 || diagnostics.Scopes[0].Overlay == nil ||
		!diagnostics.Scopes[0].Overlay.Stale {
		t.Fatalf("diagnostics overlay = %+v", diagnostics.Scopes)
	}

	plan, err := assembly.Maintain(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IgnoredOverlay {
		t.Fatalf("maintenance pass = %+v", plan)
	}
	status, err = assembly.OverlayStatus(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored || status.Stale || status.StoredProducer != status.ExpectedProducer {
		t.Fatalf("overlay status after the pass = %+v", status)
	}
}
