package recall

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
	store, err := NewStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	return store, kv
}

func testScope() corememory.Scope {
	return corememory.Scope{RuntimeID: "runtime", UserID: "user"}
}

// testEvent builds a recall event the way the provider does: one sorted,
// parallel item/score pair per returned long-term item.
func testEvent(id string, scope corememory.Scope, at time.Time, items ...string) corememory.RecallEvent {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	scores := make([]float64, len(sorted))
	for index := range scores {
		scores[index] = 0.5
	}
	return corememory.RecallEvent{ID: id, Scope: scope, ItemIDs: sorted, Scores: scores, Time: at}
}

func TestRecordCountsItemsAndIsolatesScopes(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := testScope()
	other := corememory.Scope{RuntimeID: "runtime", UserID: "other"}
	first := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	if err := store.RecordRecall(ctx, testEvent("run-1", scope, first, "item-a", "item-b")); err != nil {
		t.Fatal(err)
	}
	receipts, err := store.Receipts(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 2 {
		t.Fatalf("receipts = %#v", receipts)
	}
	for _, identity := range []string{"item-a", "item-b"} {
		receipt := receipts[identity]
		if receipt.Count != 1 || !receipt.Last.Equal(first) || receipt.LastEvent != "run-1" ||
			receipt.Identity != identity {
			t.Fatalf("receipt %q = %#v", identity, receipt)
		}
	}

	// A later event counts both again, and only the items it names.
	second := first.Add(time.Hour)
	if err := store.RecordRecall(ctx, testEvent("run-2", scope, second, "item-a")); err != nil {
		t.Fatal(err)
	}
	receipts, err = store.Receipts(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if receipt := receipts["item-a"]; receipt.Count != 2 || !receipt.Last.Equal(second) || receipt.LastEvent != "run-2" {
		t.Fatalf("second receipt = %#v", receipt)
	}
	if receipt := receipts["item-b"]; receipt.Count != 1 || !receipt.Last.Equal(first) {
		t.Fatalf("untouched receipt = %#v", receipt)
	}

	// A retry of the same event is not another recall.
	if err := store.RecordRecall(ctx, testEvent("run-2", scope, second, "item-a")); err != nil {
		t.Fatal(err)
	}
	receipts, err = store.Receipts(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if receipt := receipts["item-a"]; receipt.Count != 2 {
		t.Fatalf("retried event counted twice: %#v", receipt)
	}

	// Receipts are per scope: another scope reads none of them, even for the
	// same items.
	isolated, err := store.Receipts(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated) != 0 {
		t.Fatalf("receipts leaked into another scope: %#v", isolated)
	}
}

func TestRecordSaturatesCountAndValidatesInput(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	scope := testScope()
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	latest := base
	for index := 0; index < maxCount+32; index++ {
		at := base.Add(time.Duration(index) * time.Minute)
		latest = at
		event := testEvent(fmt.Sprintf("run-%d", index), scope, at, "item-a")
		if err := store.RecordRecall(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	receipts, err := store.Receipts(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	receipt := receipts["item-a"]
	if receipt.Count != maxCount || !receipt.Last.Equal(latest) {
		t.Fatalf("saturated receipt = %#v", receipt)
	}

	if err := store.RecordRecall(ctx, corememory.RecallEvent{ID: "run", Scope: scope, Time: base}); err == nil {
		t.Fatal("event without items accepted")
	}
	unsorted := testEvent("run", scope, base, "item-b", "item-a")
	unsorted.ItemIDs[0], unsorted.ItemIDs[1] = unsorted.ItemIDs[1], unsorted.ItemIDs[0]
	if err := store.RecordRecall(ctx, unsorted); err == nil {
		t.Fatal("unsorted event accepted")
	}
	if err := store.RecordRecall(ctx, testEvent("run", scope, base, "item/with/segments")); err == nil {
		t.Fatal("identity carrying a separator accepted")
	}
	if err := store.RecordRecall(ctx, testEvent("run", scope, base, " item")); err == nil {
		t.Fatal("untrimmed identity accepted")
	}
}

// TestReceiptsFailOnCorruptOrMisaddressedRecords pins the read's failure mode:
// a record maintenance cannot attribute is an error, not a receipt to skip.
// Skipping would decay exactly the facts the caller keeps using.
func TestReceiptsFailOnCorruptOrMisaddressedRecords(t *testing.T) {
	ctx := context.Background()
	store, kv := testStore(t)
	scope := testScope()
	prefix, err := itemsPrefix(scope)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)

	if err := kv.Put(ctx, prefix+"/item-a",
		[]byte(`{"identity":"item-other","count":1,"last":"`+stamp+`","last_event":"run-1"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Receipts(ctx, scope); err == nil || !strings.Contains(err.Error(), "names item") {
		t.Fatalf("misaddressed receipt read = %v", err)
	}
	if err := kv.Delete(ctx, prefix+"/item-a"); err != nil {
		t.Fatal(err)
	}

	if err := kv.Put(ctx, prefix+"/item-a", []byte(`{"identity":`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Receipts(ctx, scope); err == nil || !strings.Contains(err.Error(), "decode receipt") {
		t.Fatalf("undecodable receipt read = %v", err)
	}
	if err := kv.Delete(ctx, prefix+"/item-a"); err != nil {
		t.Fatal(err)
	}

	if err := kv.Put(ctx, prefix+"/item-a",
		[]byte(`{"identity":"item-a","count":2,"last":"`+stamp+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Receipts(ctx, scope); err == nil || !strings.Contains(err.Error(), "corrupt receipt") {
		t.Fatalf("receipt without an event id read = %v", err)
	}
}
