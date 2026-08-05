package entity

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

func TestEntityIndependentRecallAndStableSort(t *testing.T) {
	index, err := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "entities"})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	artifacts := []component.Artifact{
		entityArtifact("b", `["OpenAI", "海维"]`),
		entityArtifact("a", "openai, Cursor"),
		{
			Kind: "fact", ID: "skipped",
			Content: sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "OpenAI appears only in content"}}},
			Sources: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "source-skipped"}},
		},
	}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{Scope: scope, Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "OPENAI"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ID != "a" || results[1].ID != "b" || results[0].Score != 1 {
		t.Fatalf("results = %+v", results)
	}
}

func TestEntityRejectsMalformedJSON(t *testing.T) {
	index, _ := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "entities"})
	err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope:     sdkmemory.Scope{RuntimeID: "runtime"},
		Artifacts: []component.Artifact{entityArtifact("a", `["broken"`)},
	})
	if err == nil {
		t.Fatal("accepted malformed entity JSON")
	}
}

func TestEntityExtractsMentionFromNormalQueryAndTypedFact(t *testing.T) {
	index, _ := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "entities"})
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	value := entityArtifact("typed", "")
	value.Entities = []string{"OpenAI", "Sam Altman"}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{value},
	}); err != nil {
		t.Fatal(err)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{
		Scope: scope, Query: "What did Sam Altman announce at OpenAI?",
	})
	if err != nil || len(results) != 1 || results[0].ID != "typed" || results[0].Score != 1 {
		t.Fatalf("results = %+v, %v", results, err)
	}
	empty, err := index.Search(context.Background(), component.SearchRequest{
		Scope: scope, Query: "what happened yesterday?",
	})
	if err != nil || len(empty) != 0 {
		t.Fatalf("entity-free query = %+v, %v", empty, err)
	}
}

func TestEntityDatasetFilterPrecedesLimitForEveryDocumentKind(t *testing.T) {
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
			allowed := entityArtifact("allowed", `["needle"]`)
			excluded := entityArtifact("excluded", `["needle","second"]`)
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
				Scope: scope, Query: "needle second", Limit: 1,
				Metadata: sdkmemory.Metadata{"dataset_ids": `["allowed"]`},
			})
			if err != nil || len(results) != 1 || results[0].ID != "allowed" {
				t.Fatalf("results = %+v, %v", results, err)
			}
		})
	}
}

func TestEntityDeltaPersistenceTombstoneAndReconcile(t *testing.T) {
	meter := &entityMeter{Workspace: workspace.NewMemWorkspace()}
	index, err := New(Config{
		Workspace: meter, Projection: "entities",
		Thresholds: Thresholds{MaxSegments: 64, MaxDeltaBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	artifacts := make([]component.Artifact, 1000)
	for i := range artifacts {
		artifacts[i] = entityArtifact(fmt.Sprintf("item-%04d", i), "OpenAI")
		artifacts[i].Metadata["context_kind"] = string(sdkmemory.ContextDocumentChunk)
		artifacts[i].Metadata["dataset_id"] = "dataset"
		artifacts[i].Metadata["document_id"] = "document"
	}
	if err := index.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: artifacts,
	}); err != nil {
		t.Fatal(err)
	}
	meter.reset()
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, DeleteIDs: []string{artifacts[0].ID}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	if meter.written > 8192 {
		t.Fatalf("single delete wrote %d bytes", meter.written)
	}
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, ReconcileDocuments: []component.DocumentAddress{{DatasetID: "dataset", DocumentID: "document"}},
		ActiveIDs: []string{artifacts[1].ID}, SourceRevision: "r2",
	}); err != nil {
		t.Fatal(err)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "OpenAI"})
	if err != nil || len(results) != 1 || results[0].ID != artifacts[1].ID {
		t.Fatalf("reconciled results = %+v, %v", results, err)
	}
	full, err := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "entities"})
	if err != nil {
		t.Fatal(err)
	}
	if err := full.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{artifacts[1]},
	}); err != nil {
		t.Fatal(err)
	}
	fullResults, err := full.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "OpenAI"})
	if err != nil || len(fullResults) != 1 || fullResults[0].ID != results[0].ID ||
		fullResults[0].Score != results[0].Score {
		t.Fatalf("full results = %+v, %v; delta = %+v", fullResults, err, results)
	}
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, DeleteDocuments: []component.DocumentAddress{{DatasetID: "dataset", DocumentID: "document"}}, SourceRevision: "r3",
	}); err != nil {
		t.Fatal(err)
	}
	results, err = index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "OpenAI"})
	if err != nil || len(results) != 0 {
		t.Fatalf("tombstoned results = %+v, %v", results, err)
	}
}

func TestEntityReconcileIsDatasetQualified(t *testing.T) {
	index, err := New(Config{Workspace: workspace.NewMemWorkspace(), Projection: "entities"})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	makeArtifact := func(id, dataset string) component.Artifact {
		artifact := entityArtifact(id, "OpenAI")
		artifact.Metadata["context_kind"] = string(sdkmemory.ContextDocumentChunk)
		artifact.Metadata["dataset_id"] = dataset
		artifact.Metadata["document_id"] = "shared-document"
		return artifact
	}
	first, second := makeArtifact("first", "dataset-a"), makeArtifact("second", "dataset-b")
	if err := index.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{first, second},
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope:              scope,
		ReconcileDocuments: []component.DocumentAddress{{DatasetID: "dataset-a", DocumentID: "shared-document"}},
		ActiveIDs:          []string{}, SourceRevision: "reconcile-a",
	}); err != nil {
		t.Fatal(err)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "OpenAI"})
	if err != nil || len(results) != 1 || results[0].ID != second.ID {
		t.Fatalf("dataset-qualified reconcile = %#v, %v", results, err)
	}
}

type entityMeter struct {
	workspace.Workspace
	mu      sync.Mutex
	written int
}

func (meter *entityMeter) Write(ctx context.Context, name string, data []byte) error {
	meter.mu.Lock()
	meter.written += len(data)
	meter.mu.Unlock()
	return meter.Workspace.Write(ctx, name, data)
}

func (meter *entityMeter) reset() {
	meter.mu.Lock()
	meter.written = 0
	meter.mu.Unlock()
}

func entityArtifact(id, entities string) component.Artifact {
	return component.Artifact{
		Kind: "fact", ID: id,
		Content:  sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "content"}}},
		Sources:  []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "source-" + id}},
		Metadata: sdkmemory.Metadata{"conversation_id": "conversation", "entities": entities},
	}
}
