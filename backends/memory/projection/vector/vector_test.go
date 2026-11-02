package vector

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

func TestVectorUsesInferenceRuntimeAndCosineSort(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{
		KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts",
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
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

func TestVectorSearchVectorUsesStoredVectorsStableTopFiveAndShardFilter(t *testing.T) {
	// A rebuild publishes a scope's shards a few at a time, so a fake runtime
	// counts its calls atomically.
	var embedCalls atomic.Int64
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		embedCalls.Add(1)
		vectors := make([][]float32, len(texts))
		for index := range vectors {
			vectors[index] = []float32{1, 0}
		}
		return vectors
	})
	index, err := New(Config{
		KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts",
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	otherScope := corememory.Scope{RuntimeID: "runtime", UserID: "other"}
	artifacts := make([]component.Artifact, 0, 7)
	for _, id := range []string{"g", "f", "e", "d", "c", "b", "a"} {
		artifacts = append(artifacts, conversationArtifact("wanted", id, id))
	}
	excluded := conversationArtifact("other", "excluded", "excluded")
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
	before := embedCalls.Load()
	results, err := index.SearchVector(context.Background(), component.VectorSearchRequest{
		Scope: scope, Vector: []float32{1, 0}, Limit: 5,
		Filter: component.VectorSearchFilter{
			Name: "fact", Metadata: corememory.Metadata{"conversation_id": "wanted"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if after := embedCalls.Load(); after != before {
		t.Fatalf("SearchVector called embed: before=%d after=%d", before, after)
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
	kinds := []corememory.ContextItemKind{
		corememory.ContextDocumentResource,
		corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk,
		corememory.ContextDocumentSummary,
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
				KV: kvFor(t), Runtime: runtime, Model: model, Projection: "documents",
			})
			if err != nil {
				t.Fatal(err)
			}
			scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
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
					Metadata: corememory.Metadata{"dataset_ids": `["allowed"]`},
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
	index, _ := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: corememory.Scope{RuntimeID: "runtime"}, Artifacts: []component.Artifact{vectorArtifact("a", "alpha")},
	})
	if err == nil {
		t.Fatal("accepted zero vector")
	}
}

func TestVectorEmbeddingModelUsesIsolatedGeneration(t *testing.T) {
	kv := kvFor(t)
	runtime, firstModel := runtimeForModel(t, "first", func(texts []string) [][]float32 {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = []float32{1, 0}
		}
		return vectors
	})
	first, err := New(Config{KV: kv, Runtime: runtime, Model: firstModel, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := first.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Shard: shardOf("conversation"),
		Upserts: []component.Artifact{vectorArtifact("fact", "same")}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	secondModel := model.ModelRef{ID: model.ModelID{Provider: "fake", Name: "second"}}
	second, err := New(Config{KV: kv, Runtime: runtime, Model: secondModel, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	if first.storageProjection == second.storageProjection {
		t.Fatal("different embedding models shared a vector generation")
	}
	if _, err := second.readSnapshot(context.Background(), addressOf(scope, shardOf("conversation"))); err == nil {
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
	index, _ := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	scope := corememory.Scope{RuntimeID: "runtime"}
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
	var embedItems atomic.Int64
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		embedItems.Add(int64(len(texts)))
		result := make([][]float32, len(texts))
		for index := range result {
			result[index] = []float32{1, 0}
		}
		return result
	})
	index, _ := New(Config{
		KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts",
	})
	scope := corememory.Scope{RuntimeID: "runtime"}
	value := vectorArtifact("a", "same")
	value.Metadata["context_kind"] = string(corememory.ContextDocumentChunk)
	value.Metadata["dataset_id"] = "dataset"
	value.Metadata["document_id"] = "document"
	value.Metadata["item_id"] = "a"
	other := vectorArtifact("b", "other")
	other.Metadata["context_kind"] = string(corememory.ContextDocumentChunk)
	other.Metadata["dataset_id"] = "dataset"
	other.Metadata["document_id"] = "other-document"
	other.Metadata["item_id"] = "b"
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Shard: datasetShardOf("dataset"), Upserts: []component.Artifact{value, other},
	}); err != nil {
		t.Fatal(err)
	}
	value.Metadata["provenance_note"] = "changed"
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Shard: datasetShardOf("dataset"),
		Upserts: []component.Artifact{value}, ActiveIDs: []string{"a"},
	}); err != nil {
		t.Fatal(err)
	}
	if items := embedItems.Load(); items != 2 {
		t.Fatalf("unchanged content embedded %d items", items)
	}
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Shard: datasetShardOf("dataset"),
		ReconcileDocuments: []component.DocumentAddress{{DatasetID: "dataset", DocumentID: "document"}}, ActiveIDs: []string{},
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
	manifest, found, err := index.store.Audit(context.Background(),
		addressOf(scope, datasetShardOf("dataset")), index.storageProjection)
	if err != nil || !found || len(manifest.Segments) != 0 || manifest.SourceDigest == "" {
		t.Fatalf("full build manifest = %+v, found=%v, err=%v", manifest, found, err)
	}
}

func TestVectorDeltaPersistenceIsBoundedAndSearchVectorSeesSegments(t *testing.T) {
	meter := &vectorMeter{Store: kvFor(t)}
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		result := make([][]float32, len(texts))
		for i := range result {
			result[i] = []float32{1, 0}
		}
		return result
	})
	index, err := New(Config{
		KV: meter, Runtime: runtime, Model: model, Projection: "facts",
		Thresholds: Thresholds{MaxSegments: 64, MinDeltaBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
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
		Scope: scope, Shard: shardOf("conversation"),
		Upserts: []component.Artifact{changed}, SourceRevision: "r1",
	}); err != nil {
		t.Fatal(err)
	}
	if meter.written > 8192 {
		t.Fatalf("single upsert wrote %d bytes", meter.written)
	}
	meter.reset()
	if err := index.ApplyDelta(context.Background(), component.ProjectionDelta{
		Scope: scope, Shard: shardOf("conversation"),
		DeleteIDs: []string{artifacts[0].ID}, SourceRevision: "r2",
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
		KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts",
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
	storage.Store
	mu      sync.Mutex
	written int
}

func (meter *vectorMeter) Put(ctx context.Context, key string, data []byte) error {
	meter.mu.Lock()
	meter.written += len(data)
	meter.mu.Unlock()
	return meter.Store.Put(ctx, key, data)
}

func (meter *vectorMeter) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	meter.mu.Lock()
	meter.written += len(data)
	meter.mu.Unlock()
	return meter.Store.(storage.PutIfAbsentStore).PutIfAbsent(ctx, key, data)
}

func (meter *vectorMeter) reset() {
	meter.mu.Lock()
	meter.written = 0
	meter.mu.Unlock()
}

func kvFor(t *testing.T) storage.Store {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	return kvStore
}

func candidateIDs(values []component.Candidate) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].ID
	}
	return result
}

func fakeRuntime(t *testing.T) (*inference.Assembly, model.ModelRef) {
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

func runtimeFor(t *testing.T, respond func([]string) [][]float32) (*inference.Assembly, model.ModelRef) {
	return runtimeForModel(t, "embed", respond)
}

func runtimeForModel(t *testing.T, modelName string, respond func([]string) [][]float32) (*inference.Assembly, model.ModelRef) {
	t.Helper()
	driver, err := inference.BindEmbed(
		func(_ context.Context, _ model.ModelRef, request inference.EmbedRequest) (inference.Compiled[embedWire], error) {
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
				Report: inference.CompileReport{Operation: model.OperationEmbed, Decisions: decisions},
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
	ref := model.ModelRef{ID: model.ModelID{Provider: "fake", Name: modelName}}
	runtime := newTestRuntime(t, []inference.ProviderDefinition{{
		ID: "fake",
		Models: []inference.ModelImplementation{{
			Descriptor: model.ModelDescriptor{ID: ref.ID},
			Openers: inference.Openers{Embed: func(context.Context, model.ModelRef) (inference.EmbedDriver, error) {
				return driver, nil
			}},
		}},
	}})
	return runtime, ref
}

func vectorArtifact(id, text string) component.Artifact {
	return conversationArtifact("conversation", id, text)
}

// TestVectorFullRebuildPublishesShardsConcurrently pins the bound a repair
// publishes at. A rebuild embeds one shard per conversation, and embedding a
// shard is one round trip per artifact, so publishing a scope one shard at a
// time is what made a repair wait on the provider for the whole run.
func TestVectorFullRebuildPublishesShardsConcurrently(t *testing.T) {
	var (
		mutex    sync.Mutex
		inFlight int
		peak     int
	)
	runtime, model := runtimeFor(t, func(texts []string) [][]float32 {
		mutex.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mutex.Unlock()
		time.Sleep(20 * time.Millisecond)
		mutex.Lock()
		inFlight--
		mutex.Unlock()
		vectors := make([][]float32, len(texts))
		for index := range vectors {
			vectors[index] = []float32{1, 0}
		}
		return vectors
	})
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	conversations := 6
	artifacts := make([]component.Artifact, 0, conversations*3)
	for conversation := 0; conversation < conversations; conversation++ {
		id := fmt.Sprintf("conversation-%d", conversation)
		for turn := 0; turn < 3; turn++ {
			artifacts = append(artifacts, conversationArtifact(id,
				fmt.Sprintf("%s-turn-%d", id, turn), fmt.Sprintf("text %d", turn)))
		}
	}
	if err := index.FullRebuild(context.Background(),
		component.ProjectionRequest{Scope: scope, Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	flight := peak
	mutex.Unlock()
	if want := min(conversations, rebuildShardConcurrency); flight != want {
		t.Fatalf("shards in flight = %d, want %d", flight, want)
	}
	for conversation := 0; conversation < conversations; conversation++ {
		manifest, found, err := index.store.Audit(context.Background(),
			addressOf(scope, shardOf(fmt.Sprintf("conversation-%d", conversation))),
			index.storageProjection)
		if err != nil || !found || manifest.SourceDigest == "" {
			t.Fatalf("shard %d manifest = %+v, found=%v, err=%v", conversation, manifest, found, err)
		}
	}
}

// TestVectorFullRebuildStopsAtAFailedShard pins what a failing shard costs: the
// shards not yet started are left alone, and the error names the shard a repair
// has to look at.
func TestVectorFullRebuildStopsAtAFailedShard(t *testing.T) {
	conversations := rebuildShardConcurrency * 3
	names := make([]string, conversations)
	for position := range names {
		names[position] = fmt.Sprintf("conversation-%03d", position)
	}
	// The failing shard sorts first, so it is the first one a rebuild starts.
	sort.Slice(names, func(i, j int) bool { return shardOf(names[i]) < shardOf(names[j]) })
	failing := names[0]
	embedder := &failingEmbedder{failing: failing}
	index, err := New(Config{
		KV: kvFor(t), Runtime: embedder, Projection: "facts",
		Model: model.ModelRef{ID: model.ModelID{Provider: "fake", Name: "embed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	artifacts := make([]component.Artifact, 0, conversations)
	for _, name := range names {
		artifacts = append(artifacts, conversationArtifact(name, name+"-fact", "text of "+name))
	}
	err = index.FullRebuild(context.Background(),
		component.ProjectionRequest{Scope: scope, Artifacts: artifacts})
	if err == nil {
		t.Fatal("a rebuild over a failed shard reported no error")
	}
	if shard := shardOf(failing); !strings.Contains(err.Error(), shard) {
		t.Fatalf("error %q does not name shard %q", err, shard)
	}
	if embedded := embedder.count(); embedded >= conversations {
		t.Fatalf("rebuild embedded %d of %d conversations, want fewer after the failure", embedded, conversations)
	}
}

// failingEmbedder refuses the conversation it is told to refuse and embeds
// every other one, recording the conversations it was asked to embed.
type failingEmbedder struct {
	failing string
	mutex   sync.Mutex
	seen    []string
}

func (embedder *failingEmbedder) Embed(
	_ context.Context,
	_ model.ModelRef,
	request inference.EmbedRequest,
) (inference.EmbedResponse, error) {
	embedder.mutex.Lock()
	embedder.seen = append(embedder.seen, request.Items[0].Content.Text())
	embedder.mutex.Unlock()
	if strings.Contains(request.Items[0].Content.Text(), embedder.failing) {
		return inference.EmbedResponse{}, errors.New("provider refused the shard")
	}
	embeddings := make([]inference.Embedding, len(request.Items))
	for position := range embeddings {
		embeddings[position].Vector = []float32{1, 0}
	}
	return inference.EmbedResponse{Embeddings: embeddings}, nil
}

func (embedder *failingEmbedder) count() int {
	embedder.mutex.Lock()
	defer embedder.mutex.Unlock()
	return len(embedder.seen)
}

// conversationArtifact builds a fact belonging to one conversation, which is
// what decides the shard it is written to.
func conversationArtifact(conversationID, id, text string) component.Artifact {
	return component.Artifact{
		Kind: "fact", ID: id,
		Content:  coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}},
		Sources:  []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "source-" + id}},
		Metadata: corememory.Metadata{"conversation_id": conversationID},
	}
}
