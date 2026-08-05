package fact

import (
	"context"
	"reflect"
	"testing"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestWorkspaceStoreExactDedupMergesStateWithoutChangingBody(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	store := newFactStore(t, workspace.NewMemWorkspace(), WithClock(func() time.Time { return now }))
	first := factRequest("source-a", "Café likes tea")
	first.CanonicalHash = CanonicalHash("Café likes tea")
	first.Entities = []string{" Café ", "Tea"}
	first.LinkedMemoryIDs = []string{"fact-z", "source-a"}
	first.SourceDigest = "source-a"
	first.TransformSignature = "simple-v1"
	first.EventTime = now.Add(-time.Minute)
	got, err := store.Add(ctx, first)
	if err != nil {
		t.Fatal(err)
	}

	second := factRequest("source-b", "cafe\u0301  likes TEA")
	second.CanonicalHash = first.CanonicalHash
	second.Entities = []string{"tea", "café"}
	second.Provenance = []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "message-2", Revision: "2"}}
	second.LinkedMemoryIDs = []string{"fact-y"}
	second.SourceDigest = "source-b"
	second.TransformSignature = "simple-v1"
	second.EventTime = now
	merged, err := store.Add(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if merged.ID != got.ID || merged.Text != got.Text || merged.Content.Text() != got.Content.Text() {
		t.Fatalf("immutable identity/body changed: first=%#v merged=%#v", got, merged)
	}
	if len(merged.Provenance) != 2 ||
		!reflect.DeepEqual(merged.LinkedMemoryIDs, []string{"fact-y", "fact-z"}) {
		t.Fatalf("merged state = %#v", merged)
	}
	list, err := store.List(ctx, factScope, "conversation", ListOptions{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %#v, %v", list, err)
	}
	retry, err := store.Add(ctx, second)
	if err != nil || !reflect.DeepEqual(retry, merged) {
		t.Fatalf("retry = %#v, %v", retry, err)
	}
}

func TestWorkspaceStoreMergeStateSurvivesReopenAndClones(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newFactStore(t, ws)
	request := factRequest("fact", "Alice knows Bob")
	request.CanonicalHash = CanonicalHash(request.Content.Text())
	request.Entities = []string{"Alice", "Bob"}
	request.LinkedMemoryIDs = []string{"other"}
	request.SourceDigest = "digest"
	request.TransformSignature = "simple-v1"
	request.EventTime = time.Now().UTC()
	if _, err := store.Add(ctx, request); err != nil {
		t.Fatal(err)
	}
	reopened := newFactStore(t, ws)
	got, ok, err := reopened.Get(ctx, factScope, "conversation", request.ID)
	if err != nil || !ok {
		t.Fatalf("get = %#v, %v, %v", got, ok, err)
	}
	got.Entities[0] = "mutated"
	got.LinkedMemoryIDs[0] = "mutated"
	again, _, _ := reopened.Get(ctx, factScope, "conversation", request.ID)
	if again.Entities[0] != "alice" || again.LinkedMemoryIDs[0] != "other" {
		t.Fatalf("fact aliases caller: %#v", again)
	}
}
