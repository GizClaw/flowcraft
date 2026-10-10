package projection

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func TestPublishFailureRetainsOldActiveBuild(t *testing.T) {
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	fault := &faultKV{Store: kvStore}
	store, err := NewTypedStore(fault, "test", TypedOptions[map[string]bool, map[string]bool]{
		Apply: func(base *map[string]bool, delta map[string]bool) error {
			for key, value := range delta {
				(*base)[key] = value
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := store.FullRebuild(context.Background(), scope, "index", map[string]bool{"old": true}, "old"); err != nil {
		t.Fatal(err)
	}
	fault.failActive = true
	if err := store.FullRebuild(context.Background(), scope, "index", map[string]bool{"new": true}, "new"); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	fault.failActive = false
	data, _, err := store.Materialize(context.Background(), scope, "index")
	if err != nil {
		t.Fatal(err)
	}
	if !data["old"] || data["new"] {
		t.Fatalf("active data = %+v", data)
	}
}

func TestAuditDigestEvidenceSeparatesStoredAndComputedDigests(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewTypedStore(kvStore, "test", TypedOptions[map[string]bool, map[string]bool]{
		Apply: func(base *map[string]bool, delta map[string]bool) error {
			for key, value := range delta {
				(*base)[key] = value
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := store.FullRebuild(ctx, scope, "index", map[string]bool{"value": true}, "source"); err != nil {
		t.Fatal(err)
	}
	evidence, found, err := store.AuditDigestEvidence(ctx, scope, "index")
	if err != nil || !found {
		t.Fatalf("normal audit = %#v, %v, %v", evidence, found, err)
	}
	if evidence.StoredSourceDigest != evidence.ComputedSourceDigest ||
		evidence.StoredBuildDigest != evidence.ComputedBuildDigest {
		t.Fatalf("normal audit mismatched = %#v", evidence)
	}

	data, err := kvStore.Get(ctx, store.activePath(scope, "index"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.SourceDigest = "tampered-source"
	manifest.BuildDigest = "tampered-build"
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := kvStore.Put(ctx, store.activePath(scope, "index"), data); err != nil {
		t.Fatal(err)
	}

	evidence, found, err = store.AuditDigestEvidence(ctx, scope, "index")
	if err != nil || !found {
		t.Fatalf("tampered audit = %#v, %v, %v", evidence, found, err)
	}
	if evidence.StoredSourceDigest == evidence.ComputedSourceDigest ||
		evidence.StoredBuildDigest == evidence.ComputedBuildDigest {
		t.Fatalf("tampered audit did not expose mismatch = %#v", evidence)
	}
}

func TestMatchesRequestAppliesDatasetOnlyToDocumentKinds(t *testing.T) {
	for _, kind := range []corememory.ContextItemKind{
		corememory.ContextDocumentResource,
		corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk,
		corememory.ContextDocumentSummary,
	} {
		t.Run(string(kind), func(t *testing.T) {
			matches, err := MatchesRequest(
				corememory.Metadata{"dataset_ids": `["allowed"]`},
				component.CandidateAddress{Kind: kind, DatasetID: "excluded"},
			)
			if err != nil || matches {
				t.Fatalf("matches=%v err=%v", matches, err)
			}
		})
	}
	for _, kind := range []corememory.ContextItemKind{corememory.ContextRawMessage, corememory.ContextFact} {
		t.Run(string(kind), func(t *testing.T) {
			matches, err := MatchesRequest(
				corememory.Metadata{"dataset_ids": `["allowed"]`},
				component.CandidateAddress{Kind: kind, ConversationID: "conversation"},
			)
			if err != nil || !matches {
				t.Fatalf("matches=%v err=%v", matches, err)
			}
		})
	}
}

// TestSelectorKeyCoversMatchesRequestKeys couples the read-side selectors
// to the fingerprint that callers cache a selection under: flipping any
// metadata key MatchesRequest consults must flip the fingerprint, and keys
// it ignores must not. A key added to MatchesRequest without being added to
// SelectorKey — and to this list — would let a cached selection outlive the
// request it was built for.
func TestSelectorKeyCoversMatchesRequestKeys(t *testing.T) {
	selector := func(conversation, datasets string) corememory.Metadata {
		return corememory.Metadata{"conversation_id": conversation, "dataset_ids": datasets}
	}
	base := selector("c1", `["d1"]`)
	if SelectorKey(base) != SelectorKey(selector("c1", `["d1"]`)) {
		t.Fatal("fingerprint is not stable for identical selectors")
	}
	for name, flipped := range map[string]corememory.Metadata{
		"conversation_id": selector("c2", `["d1"]`),
		"dataset_ids":     selector("c1", `["d2"]`),
	} {
		if SelectorKey(flipped) == SelectorKey(base) {
			t.Errorf("fingerprint ignores %s, which MatchesRequest consults", name)
		}
	}
	if SelectorKey(corememory.Metadata{"unrelated": "x"}) != SelectorKey(nil) {
		t.Error("fingerprint changes for metadata that does not narrow a read")
	}
	// The coupling itself, over a matrix of selectors and addresses rather
	// than the two keys the fingerprint happens to read today: whenever
	// MatchesRequest answers two requests differently for one address, the
	// fingerprint has to separate them too. A key MatchesRequest consults
	// but SelectorKey ignores — one added later, like the generation_id the
	// provider also puts in metadata — would let a cached selection outlive
	// the request it was built for.
	addresses := []component.CandidateAddress{
		{Kind: corememory.ContextRawMessage, ConversationID: "c1"},
		{Kind: corememory.ContextRawMessage, ConversationID: "c2"},
		{Kind: corememory.ContextDocumentChunk, DatasetID: "d1"},
		{Kind: corememory.ContextDocumentChunk, DatasetID: "d2"},
	}
	variants := []corememory.Metadata{
		nil,
		{"conversation_id": "c1"},
		{"conversation_id": "c2"},
		{"dataset_ids": `["d1"]`},
		{"dataset_ids": `["d2"]`},
		{"conversation_id": "c1", "dataset_ids": `["d1"]`},
		{"conversation_id": "c1", "generation_id": "generation-1"},
	}
	for _, left := range variants {
		for _, right := range variants {
			for _, address := range addresses {
				leftMatches, leftErr := MatchesRequest(left, address)
				rightMatches, rightErr := MatchesRequest(right, address)
				if leftErr != nil || rightErr != nil {
					t.Fatalf("MatchesRequest(%v, %+v): %v, %v", left, address, leftErr, rightErr)
				}
				if leftMatches != rightMatches && SelectorKey(left) == SelectorKey(right) {
					t.Errorf("SelectorKey(%v) == SelectorKey(%v), but MatchesRequest differs for %+v",
						left, right, address)
				}
			}
		}
	}
}

type faultKV struct {
	storage.Store
	failActive bool
}

func (kv *faultKV) Put(ctx context.Context, key string, data []byte) error {
	if kv.failActive && strings.HasSuffix(key, "/active.json") {
		return errors.New("injected publish failure")
	}
	return kv.Store.Put(ctx, key, data)
}

func (kv *faultKV) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	return kv.Store.(storage.PutIfAbsentStore).PutIfAbsent(ctx, key, data)
}
