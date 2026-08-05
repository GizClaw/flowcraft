package vector

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestVectorUsesInferenceRuntimeAndCosineSort(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{
		Workspace: workspace.NewMemWorkspace(), Runtime: runtime, Model: model, Projection: "facts",
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	artifacts := []component.Artifact{
		vectorArtifact("b", "beta"),
		vectorArtifact("c", "alpha duplicate"),
		vectorArtifact("a", "alpha"),
	}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{Scope: scope, Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].ID != "a" || results[0].Score != 1 ||
		results[1].ID != "c" || results[2].ID != "b" {
		t.Fatalf("results = %+v", results)
	}
}

func TestVectorSearchVectorUsesStoredVectorsStableTopFiveAndScopeFilter(t *testing.T) {
	embedCalls := 0
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		embedCalls++
		vectors := make([][]float32, len(texts))
		for index := range vectors {
			vectors[index] = []float32{1, 0}
		}
		return vectors
	})
	index, err := New(Config{
		Workspace: workspace.NewMemWorkspace(), Runtime: runtime, Model: model, Projection: "facts",
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	otherScope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "other"}
	artifacts := make([]component.Artifact, 0, 7)
	for _, id := range []string{"g", "f", "e", "d", "c", "b", "a"} {
		artifact := vectorArtifact(id, id)
		artifact.Metadata["conversation_id"] = "wanted"
		artifacts = append(artifacts, artifact)
	}
	excluded := vectorArtifact("excluded", "excluded")
	excluded.Metadata["conversation_id"] = "other"
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: append(artifacts, excluded),
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: otherScope, Artifacts: []component.Artifact{vectorArtifact("foreign", "foreign")},
	}); err != nil {
		t.Fatal(err)
	}
	before := embedCalls
	results, err := index.SearchVector(context.Background(), component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0}, Limit: 5,
		Filter: component.VectorSearchFilter{
			Name: "fact", Metadata: sdkmemory.Metadata{"conversation_id": "wanted"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if embedCalls != before {
		t.Fatalf("SearchVector called embed: before=%d after=%d", before, embedCalls)
	}
	got := make([]string, len(results))
	for index := range results {
		got[index] = results[index].ID
	}
	if strings.Join(got, ",") != "a,b,c,d,e" {
		t.Fatalf("stable top five = %v", got)
	}
}

func TestVectorDatasetFilterPrecedesLimitForEveryDocumentKind(t *testing.T) {
	kinds := []sdkmemory.ContextItemKind{
		sdkmemory.ContextDocumentResource,
		sdkmemory.ContextDocumentSection,
		sdkmemory.ContextDocumentChunk,
		sdkmemory.ContextDocumentSummary,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
				vectors := make([][]float32, len(texts))
				for index, text := range texts {
					switch text {
					case "allowed":
						vectors[index] = []float32{0.8, 0.6}
					default:
						vectors[index] = []float32{1, 0}
					}
				}
				return vectors
			})
			index, err := New(Config{
				Workspace: workspace.NewMemWorkspace(), Runtime: runtime, Model: model, Projection: "documents",
			})
			if err != nil {
				t.Fatal(err)
			}
			scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
			allowed := vectorArtifact("allowed", "allowed")
			excluded := vectorArtifact("excluded", "excluded")
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
			results, err := index.SearchVector(context.Background(), component.VectorSearchRequest{
				Scope: scope, Vector: []float32{1, 0}, Limit: 1,
				Filter: component.VectorSearchFilter{
					Metadata: sdkmemory.Metadata{"dataset_ids": `["allowed"]`},
				},
			})
			if err != nil || len(results) != 1 || results[0].ID != "allowed" {
				t.Fatalf("results = %+v, %v", results, err)
			}
		})
	}
}

func TestVectorRejectsZeroVector(t *testing.T) {
	runtime, model := runtimeFor(t, func([]string) [][]float32 {
		return [][]float32{{0, 0}}
	})
	index, _ := New(Config{Workspace: workspace.NewMemWorkspace(), Runtime: runtime, Model: model, Projection: "facts"})
	err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: sdkmemory.Scope{RuntimeID: "runtime"}, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
	})
	if err == nil {
		t.Fatal("accepted zero vector")
	}
}

func TestVectorEmbeddingModelUsesIsolatedGeneration(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	runtime, firstModel := runtimeForModel(t, "first", func(texts []string) [][]float32 {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = []float32{1, 0}
		}
		return vectors
	})
	first, err := New(Config{Workspace: ws, Runtime: runtime, Model: firstModel, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	if err := first.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Upserts: []component.Artifact{vectorArtifact("fact", "same")}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	secondModel := inference.ModelRef{ID: inference.ModelID{Provider: "fake", Name: "second"}}
	second, err := New(Config{Workspace: ws, Runtime: runtime, Model: secondModel, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	if first.storageProjection == second.storageProjection {
		t.Fatal("different embedding models shared a vector generation")
	}
	if _, err := second.readSnapshot(context.Background(), scope); err == nil {
		t.Fatal("new embedding model read the previous model generation")
	}
}

func TestVectorRejectsQueryDimensionMismatch(t *testing.T) {
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			if text == "query" {
				vectors[i] = []float32{1, 0, 0}
			} else {
				vectors[i] = []float32{1, 0}
			}
		}
		return vectors
	})
	index, _ := New(Config{Workspace: workspace.NewMemWorkspace(), Runtime: runtime, Model: model, Projection: "facts"})
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "query"}); err == nil {
		t.Fatal("accepted query dimension mismatch")
	}
}

func TestVectorDeltaSkipsUnchangedEmbeddingDeletesTombstoneAndFullRebuilds(t *testing.T) {
	embedItems := 0
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		embedItems += len(texts)
		result := make([][]float32, len(texts))
		for index := range result {
			result[index] = []float32{1, 0}
		}
		return result
	})
	index, _ := New(Config{
		Workspace: workspace.NewMemWorkspace(), Runtime: runtime, Model: model, Projection: "facts",
	})
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	value := vectorArtifact("a", "same")
	value.Metadata["context_kind"] = string(sdkmemory.ContextDocumentChunk)
	value.Metadata["dataset_id"] = "dataset"
	value.Metadata["document_id"] = "document"
	value.Metadata["item_id"] = "a"
	other := vectorArtifact("b", "other")
	other.Metadata["context_kind"] = string(sdkmemory.ContextDocumentChunk)
	other.Metadata["dataset_id"] = "dataset"
	other.Metadata["document_id"] = "other-document"
	other.Metadata["item_id"] = "b"
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Upserts: []component.Artifact{value, other},
	}); err != nil {
		t.Fatal(err)
	}
	value.Metadata["provenance_note"] = "changed"
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Upserts: []component.Artifact{value}, ActiveIDs: []string{"a"},
	}); err != nil {
		t.Fatal(err)
	}
	if embedItems != 2 {
		t.Fatalf("unchanged content embedded %d items", embedItems)
	}
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, ReconcileDocuments: []component.DocumentAddress{{DatasetID: "dataset", DocumentID: "document"}}, ActiveIDs: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	results, err := index.Search(context.Background(), component.SearchRequest{Scope: scope, Query: "same"})
	if err != nil || len(results) != 1 || results[0].ID != "b" {
		t.Fatalf("tombstone results = %+v, %v", results, err)
	}
	if err := index.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{value},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, found, err := index.store.Audit(context.Background(), scope, index.storageProjection)
	if err != nil || !found || len(manifest.Segments) != 0 || manifest.SourceDigest == "" {
		t.Fatalf("full build manifest = %+v, found=%v, err=%v", manifest, found, err)
	}
}

func TestVectorDeltaPersistenceIsBoundedAndSearchVectorSeesSegments(t *testing.T) {
	meter := &vectorMeter{Workspace: workspace.NewMemWorkspace()}
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		result := make([][]float32, len(texts))
		for i := range result {
			result[i] = []float32{1, 0}
		}
		return result
	})
	index, err := New(Config{
		Workspace: meter, Runtime: runtime, Model: model, Projection: "facts",
		Thresholds: Thresholds{MaxSegments: 64, MaxDeltaBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	artifacts := make([]component.Artifact, 1000)
	for i := range artifacts {
		artifacts[i] = vectorArtifact(fmt.Sprintf("item-%04d", i), "original")
	}
	if err := index.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: artifacts,
	}); err != nil {
		t.Fatal(err)
	}
	meter.reset()
	changed := vectorArtifact("changed", "updated")
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
	results, err := index.SearchVector(context.Background(), component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
	})
	if err != nil || len(results) != 1000 {
		t.Fatalf("SearchVector results=%d, err=%v", len(results), err)
	}
	full, err := New(Config{
		Workspace: workspace.NewMemWorkspace(), Runtime: runtime, Model: model, Projection: "facts",
	})
	if err != nil {
		t.Fatal(err)
	}
	finalArtifacts := append(component.CloneArtifacts(artifacts[1:]), changed)
	if err := full.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: finalArtifacts,
	}); err != nil {
		t.Fatal(err)
	}
	fullResults, err := full.SearchVector(context.Background(), component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidateIDs(results), candidateIDs(fullResults)) {
		t.Fatal("ordered vector deltas differ from full rebuild")
	}
}

type vectorMeter struct {
	workspace.Workspace
	mu      sync.Mutex
	written int
}

func (meter *vectorMeter) Write(ctx context.Context, name string, data []byte) error {
	meter.mu.Lock()
	meter.written += len(data)
	meter.mu.Unlock()
	return meter.Workspace.Write(ctx, name, data)
}

func (meter *vectorMeter) reset() {
	meter.mu.Lock()
	meter.written = 0
	meter.mu.Unlock()
}

func candidateIDs(values []component.Candidate) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].ID
	}
	return result
}

func fakeRuntime(t *testing.T) (*inference.Runtime, inference.ModelRef) {
	t.Helper()
	return runtimeFor(t, func(texts []string) [][]float32 {
		vectors := make([][]float32, len(texts))
		for i, text := range texts {
			if strings.Contains(strings.ToLower(text), "alpha") {
				vectors[i] = []float32{1, 0}
			} else {
				vectors[i] = []float32{0, 1}
			}
		}
		return vectors
	})
}

type embedWire struct {
	Texts []string
}

type embedRaw struct {
	Vectors [][]float32
}

func runtimeFor(t *testing.T, respond func([]string) [][]float32) (*inference.Runtime, inference.ModelRef) {
	return runtimeForModel(t, "embed", respond)
}

func runtimeForModel(t *testing.T, modelName string, respond func([]string) [][]float32) (*inference.Runtime, inference.ModelRef) {
	t.Helper()
	driver, err := inference.BindEmbed(
		func(_ context.Context, _ inference.ModelRef, request inference.EmbedRequest) (inference.Compiled[embedWire], error) {
			decisions := make([]inference.Decision, 0)
			for _, field := range request.ActiveFields() {
				decisions = append(decisions, inference.Decision{Field: field, Disposition: inference.Native})
			}
			texts := make([]string, len(request.Items))
			for i, item := range request.Items {
				texts[i] = item.Content.Text()
			}
			return inference.Compiled[embedWire]{
				Wire:   embedWire{Texts: texts},
				Report: inference.CompileReport{Operation: inference.OperationEmbed, Decisions: decisions},
			}, nil
		},
		func(_ context.Context, request embedWire) (embedRaw, error) {
			return embedRaw{Vectors: respond(request.Texts)}, nil
		},
		func(_ context.Context, response embedRaw) (inference.EmbedResponse, error) {
			embeddings := make([]inference.Embedding, len(response.Vectors))
			for i, vector := range response.Vectors {
				embeddings[i].Vector = vector
			}
			return inference.EmbedResponse{Embeddings: embeddings}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	model := inference.ModelRef{ID: inference.ModelID{Provider: "fake", Name: modelName}}
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "fake",
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{ID: model.ID},
			Openers: inference.Openers{Embed: func(context.Context, inference.ModelRef) (inference.EmbedDriver, error) {
				return driver, nil
			}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, model
}

func vectorArtifact(id, text string) component.Artifact {
	return component.Artifact{
		Kind: "fact", ID: id,
		Content:  sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}},
		Sources:  []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "source-" + id}},
		Metadata: sdkmemory.Metadata{"conversation_id": "conversation"},
	}
}
