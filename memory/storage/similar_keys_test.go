package storage

import (
	"context"
	"fmt"
	"testing"
)

// TestWorkspaceKVSimilarKeys guards against key-to-path collisions between
// many keys that share a long common prefix.
func TestWorkspaceKVSimilarKeys(t *testing.T) {
	store, err := NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keys := make([]string, 0, 65)
	for index := 0; index < 65; index++ {
		id := fmt.Sprintf("summary-%06d", index)
		key := "views/summary/v1/records/" + EncodeSegment(id) + ".json"
		keys = append(keys, key)
		if err := store.Put(ctx, key, []byte(id)); err != nil {
			t.Fatal(err)
		}
	}
	for index, key := range keys {
		id := fmt.Sprintf("summary-%06d", index)
		got, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("get %q: %v", key, err)
		}
		if string(got) != id {
			t.Fatalf("get %q = %q, want %q", key, got, id)
		}
	}
}
