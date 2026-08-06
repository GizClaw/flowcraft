package projection

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestPublishFailureRetainsOldActiveBuild(t *testing.T) {
	base := workspace.NewMemWorkspace()
	fault := &faultWorkspace{Workspace: base}
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
	fault.failActiveRename = true
	if err := store.FullRebuild(context.Background(), scope, "index", map[string]bool{"new": true}, "new"); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	fault.failActiveRename = false
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
	ws := workspace.NewMemWorkspace()
	store, err := NewTypedStore(ws, "test", TypedOptions[map[string]bool, map[string]bool]{
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

	data, err := ws.Read(ctx, store.activePath(scope, "index"))
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
	ws.MustWrite(store.activePath(scope, "index"), data)

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

type faultWorkspace struct {
	workspace.Workspace
	failActiveRename bool
}

func (workspace *faultWorkspace) Rename(ctx context.Context, source, destination string) error {
	if workspace.failActiveRename && strings.HasSuffix(destination, "/active.json") {
		return errors.New("injected publish failure")
	}
	return workspace.Workspace.Rename(ctx, source, destination)
}

// Compile-time documentation that the fault wrapper remains a full Workspace.
var _ interface {
	Read(context.Context, string) ([]byte, error)
	Write(context.Context, string, []byte) error
	Append(context.Context, string, []byte) error
	Rename(context.Context, string, string) error
	Delete(context.Context, string) error
	RemoveAll(context.Context, string) error
	List(context.Context, string) ([]fs.DirEntry, error)
	Exists(context.Context, string) (bool, error)
	Stat(context.Context, string) (fs.FileInfo, error)
} = (*faultWorkspace)(nil)
