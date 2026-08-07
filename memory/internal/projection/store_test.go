package projection

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestPublishFailureRetainsOldActiveBuild(t *testing.T) {
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
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
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
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
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
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
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
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
	for _, kind := range []sdkmemory.ContextItemKind{
		sdkmemory.ContextDocumentResource,
		sdkmemory.ContextDocumentSection,
		sdkmemory.ContextDocumentChunk,
		sdkmemory.ContextDocumentSummary,
	} {
		t.Run(string(kind), func(t *testing.T) {
			matches, err := MatchesRequest(
				sdkmemory.Metadata{"dataset_ids": `["allowed"]`},
				component.CandidateAddress{Kind: kind, DatasetID: "excluded"},
			)
			if err != nil || matches {
				t.Fatalf("matches=%v err=%v", matches, err)
			}
		})
	}
	for _, kind := range []sdkmemory.ContextItemKind{sdkmemory.ContextRawMessage, sdkmemory.ContextFact} {
		t.Run(string(kind), func(t *testing.T) {
			matches, err := MatchesRequest(
				sdkmemory.Metadata{"dataset_ids": `["allowed"]`},
				component.CandidateAddress{Kind: kind, ConversationID: "conversation"},
			)
			if err != nil || !matches {
				t.Fatalf("matches=%v err=%v", matches, err)
			}
		})
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
