package bm25

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestBM25HappyPathUnicodeAndStableTie(t *testing.T) {
	index, err := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "facts", K1: 1.2, B: 0.75})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	artifacts := []component.Artifact{
		artifact("b", "GOPHER 世界"),
		artifact("a", "gopher 世界"),
		artifact("c", "unrelated"),
	}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{Scope: scope, Projection: "facts", Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "GoPhEr 世界"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ID != "a" || results[1].ID != "b" || results[0].Score <= 0 {
		t.Fatalf("results = %+v", results)
	}
}

func TestBM25RejectsParameters(t *testing.T) {
	if _, err := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "x", K1: -1}); err == nil {
		t.Fatal("accepted negative k1")
	}
	if _, err := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "x", K1: 1, B: 2}); err == nil {
		t.Fatal("accepted b > 1")
	}
}

func TestBM25EmptyQueryReturnsEmpty(t *testing.T) {
	index, _ := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "facts"})
	results, err := index.Search(context.Background(), component.SearchRequest{
		Scope: sdkmemory.Scope{RuntimeID: "runtime"}, Query: " \t ",
	})
	if err != nil || len(results) != 0 {
		t.Fatalf("empty query = %+v, %v", results, err)
	}
}

func TestBM25DatasetFilterPrecedesLimitForEveryDocumentKind(t *testing.T) {
	kinds := []sdkmemory.ContextItemKind{
		sdkmemory.ContextDocumentResource,
		sdkmemory.ContextDocumentSection,
		sdkmemory.ContextDocumentChunk,
		sdkmemory.ContextDocumentSummary,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			index, err := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "documents"})
			if err != nil {
				t.Fatal(err)
			}
			scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
			allowed := artifact("allowed", "needle")
			excluded := artifact("excluded", "needle needle needle needle")
			for _, item := range []*component.Artifact{&allowed, &excluded} {
				item.Kind = component.ArtifactKind(kind)
				item.Metadata["context_kind"] = string(kind)
				item.Metadata["document_id"] = "document-" + item.ID
				item.Metadata["item_id"] = item.ID
			}
			allowed.Metadata["dataset_id"] = "allowed"
			excluded.Metadata["dataset_id"] = "excluded"
			if err := index.Rebuild(context.Background(), component.ProjectionRequest{
				Scope: scope, Projection: "documents", Artifacts: []component.Artifact{allowed, excluded},
			}); err != nil {
				t.Fatal(err)
			}
			results, err := index.Search(context.Background(), component.SearchRequest{
				Scope: scope, Query: "needle", Limit: 1,
				Metadata: sdkmemory.Metadata{"dataset_ids": `["allowed"]`},
			})
			if err != nil || len(results) != 1 || results[0].ID != "allowed" {
				t.Fatalf("results = %+v, %v", results, err)
			}
		})
	}
}

func TestBM25DeltaPersistenceIsBoundedAndMatchesFullRebuild(t *testing.T) {
	meter := &bm25Meter{Workspace: workspace.NewMemWorkspace()}
	index, err := New(Config{
		Workspace: meter, Projection: "facts",
		Thresholds: Thresholds{MaxSegments: 64, MaxDeltaBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	artifacts := make([]component.Artifact, 1000)
	for i := range artifacts {
		artifacts[i] = artifact(fmt.Sprintf("item-%04d", i), "common original")
	}
	if err := index.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: artifacts,
	}); err != nil {
		t.Fatal(err)
	}
	meter.reset()
	changed := artifact("changed", "common updated")
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Upserts: []component.Artifact{changed}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	if meter.written > 8192 {
		t.Fatalf("single upsert wrote %d bytes", meter.written)
	}
	meter.reset()
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, DeleteIDs: []string{artifacts[0].ID}, SourceRevision: "r2",
	}); err != nil {
		t.Fatal(err)
	}
	if meter.written > 8192 {
		t.Fatalf("single delete wrote %d bytes", meter.written)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "updated"})
	if err != nil || len(results) != 1 || results[0].ID != "changed" {
		t.Fatalf("delta results = %+v, %v", results, err)
	}
	full, err := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	if err := full.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: append(component.CloneArtifacts(artifacts[1:]), changed),
	}); err != nil {
		t.Fatal(err)
	}
	fullResults, err := full.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "updated"})
	if err != nil || len(fullResults) != 1 || fullResults[0].ID != results[0].ID ||
		fullResults[0].Score != results[0].Score {
		t.Fatalf("full results = %+v, %v; delta = %+v", fullResults, err, results)
	}
}

type bm25Meter struct {
	workspace.Workspace
	mu      sync.Mutex
	written int
}

func (meter *bm25Meter) Write(ctx context.Context, name string, data []byte) error {
	meter.mu.Lock()
	meter.written += len(data)
	meter.mu.Unlock()
	return meter.Workspace.Write(ctx, name, data)
}

func (meter *bm25Meter) reset() {
	meter.mu.Lock()
	meter.written = 0
	meter.mu.Unlock()
}

func artifact(id, text string) component.Artifact {
	return component.Artifact{
		Kind: "fact", ID: id,
		Content:  sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}},
		Sources:  []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "source-" + id}},
		Metadata: sdkmemory.Metadata{"conversation_id": "conversation"},
	}
}
