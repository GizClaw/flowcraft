// Package vector implements the embedding retrieval projection.
package vector

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/memory/internal/projection"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const (
	laneName                = "vector"
	AlgorithmVersion        = "vector-cosine-v2"
	StorageAlgorithmVersion = projectionstore.StorageAlgorithmVersion
)

type Thresholds = projectionstore.Thresholds

func DefaultThresholds() Thresholds { return projectionstore.DefaultThresholds() }

type Config struct {
	Workspace  workspace.Workspace
	Runtime    *inference.Runtime
	Model      inference.ModelRef
	Projection string
	Thresholds projectionstore.Thresholds
}

type Index struct {
	store             *projectionstore.Store[snapshot, projectionstore.EntryDelta[entry]]
	runtime           *inference.Runtime
	model             inference.ModelRef
	projection        string
	storageProjection string
}

// AuditDigests independently recomputes and validates active projection digests.
func (index *Index) AuditDigests(
	ctx context.Context,
	scope sdkmemory.Scope,
) (string, string, string, string, bool, error) {
	evidence, found, err := index.store.AuditDigestEvidence(ctx, scope, index.storageProjection)
	return evidence.StoredSourceDigest, evidence.ComputedSourceDigest,
		evidence.StoredBuildDigest, evidence.ComputedBuildDigest, found, err
}

type snapshot struct {
	Dimensions int     `json:"dimensions"`
	Entries    []entry `json:"entries"`
}

type entry struct {
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Vector        []float32                  `json:"vector"`
	Source        sdkmemory.SourceRef        `json:"source"`
	Address       component.CandidateAddress `json:"address"`
	Metadata      sdkmemory.Metadata         `json:"metadata,omitempty"`
	ContentDigest string                     `json:"content_digest"`
}

var _ component.Indexer = (*Index)(nil)
var _ component.DeltaIndexer = (*Index)(nil)
var _ component.FullRebuilder = (*Index)(nil)
var _ component.Searcher = (*Index)(nil)
var _ component.VectorSearcher = (*Index)(nil)

func New(config Config) (*Index, error) {
	if config.Runtime == nil {
		return nil, errors.New("vector projection: inference runtime is required")
	}
	if err := config.Model.Validate(); err != nil {
		return nil, fmt.Errorf("vector projection: model: %w", err)
	}
	if strings.TrimSpace(config.Projection) == "" {
		return nil, errors.New("vector projection: projection name is required")
	}
	key := vectorEntryKey()
	store, err := projectionstore.NewTypedStore(config.Workspace, laneName,
		projectionstore.TypedOptions[snapshot, projectionstore.EntryDelta[entry]]{
			Thresholds: config.Thresholds,
			Canonicalize: func(delta projectionstore.EntryDelta[entry]) projectionstore.EntryDelta[entry] {
				return projectionstore.CanonicalEntryDelta(delta, key)
			},
			ValidateBase: validateSnapshot,
			Apply: func(base *snapshot, delta projectionstore.EntryDelta[entry]) error {
				base.Entries = projectionstore.ApplyEntryDelta(base.Entries, delta, key)
				return recomputeDimensions(base)
			},
		})
	if err != nil {
		return nil, fmt.Errorf("vector projection: %w", err)
	}
	return &Index{
		store: store, runtime: config.Runtime, model: config.Model, projection: config.Projection,
		storageProjection: generationProjection(config.Projection, config.Model),
	}, nil
}

func (index *Index) Rebuild(ctx context.Context, request component.ProjectionRequest) error {
	return index.FullRebuild(ctx, request)
}

func (index *Index) FullRebuild(ctx context.Context, request component.ProjectionRequest) error {
	if err := index.validateRequest(request); err != nil {
		return err
	}
	if len(request.Artifacts) == 0 {
		return index.store.FullRebuild(ctx, request.Scope, index.storageProjection, snapshot{}, digestArtifacts(request.Artifacts))
	}
	items := make([]inference.EmbedItem, len(request.Artifacts))
	for i, artifact := range request.Artifacts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("vector projection: artifact %d: %w", i, err)
		}
		items[i] = inference.EmbedItem{Content: artifact.Content.Clone()}
	}
	response, err := index.runtime.Embed(ctx, index.model, inference.EmbedRequest{Items: items})
	if err != nil {
		return fmt.Errorf("vector projection: embed artifacts: %w", err)
	}
	if err := response.ValidateFor(inference.EmbedRequest{Items: items}); err != nil {
		return fmt.Errorf("vector projection: invalid embedding response: %w", err)
	}
	entries := make([]entry, len(request.Artifacts))
	dimensions := len(response.Embeddings[0].Vector)
	for i, artifact := range request.Artifacts {
		vector := append([]float32(nil), response.Embeddings[i].Vector...)
		if _, err := norm(vector); err != nil {
			return fmt.Errorf("vector projection: artifact %q: %w", artifact.ID, err)
		}
		address := component.AddressFromArtifact(artifact)
		if err := address.Validate(); err != nil {
			return fmt.Errorf("vector projection: artifact %q address: %w", artifact.ID, err)
		}
		entries[i] = entry{
			ID: artifact.ID, Name: string(artifact.Kind), Vector: vector,
			Source: artifact.Sources[0], Address: address,
			Metadata:      artifact.Metadata.Clone(),
			ContentDigest: contentDigest(artifact),
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return index.store.FullRebuild(ctx, request.Scope, index.storageProjection,
		snapshot{Dimensions: dimensions, Entries: entries}, digestArtifacts(request.Artifacts))
}

func (index *Index) ApplyDelta(ctx context.Context, delta component.ProjectionDelta) error {
	if err := delta.ValidateDocumentAddresses(); err != nil {
		return fmt.Errorf("vector projection: %w", err)
	}
	request := component.ProjectionRequest{Scope: delta.Scope, Projection: delta.Projection}
	if err := index.validateRequest(request); err != nil {
		return err
	}
	build, _, err := index.store.Materialize(ctx, delta.Scope, index.storageProjection)
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("vector projection: materialize active build: %w", err)
	}
	entries := make(map[string]entry, len(build.Entries))
	for _, item := range build.Entries {
		entries[item.ID] = item
	}
	changed := make([]component.Artifact, 0, len(delta.Upserts))
	for _, artifact := range delta.Upserts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("vector projection: artifact %q: %w", artifact.ID, err)
		}
		digest := contentDigest(artifact)
		if prior, ok := entries[artifact.ID]; !ok || prior.ContentDigest != digest {
			changed = append(changed, artifact)
		}
	}
	vectors := make(map[string][]float32, len(changed))
	if len(changed) > 0 {
		items := make([]inference.EmbedItem, len(changed))
		for i, artifact := range changed {
			items[i] = inference.EmbedItem{Content: artifact.Content.Clone()}
		}
		response, err := index.runtime.Embed(ctx, index.model, inference.EmbedRequest{Items: items})
		if err != nil {
			return fmt.Errorf("vector projection: embed delta: %w", err)
		}
		if err := response.ValidateFor(inference.EmbedRequest{Items: items}); err != nil {
			return fmt.Errorf("vector projection: invalid delta embeddings: %w", err)
		}
		for i, artifact := range changed {
			if _, err := norm(response.Embeddings[i].Vector); err != nil {
				return fmt.Errorf("vector projection: artifact %q: %w", artifact.ID, err)
			}
			vectors[artifact.ID] = append([]float32(nil), response.Embeddings[i].Vector...)
		}
	}
	upserts := make([]entry, 0, len(delta.Upserts))
	for _, artifact := range delta.Upserts {
		address := component.AddressFromArtifact(artifact)
		if err := address.Validate(); err != nil {
			return fmt.Errorf("vector projection: artifact %q address: %w", artifact.ID, err)
		}
		vector := vectors[artifact.ID]
		if vector == nil {
			vector = append([]float32(nil), entries[artifact.ID].Vector...)
		}
		upserts = append(upserts, entry{
			ID: artifact.ID, Name: string(artifact.Kind), Vector: vector,
			Source: artifact.Sources[0], Address: address, Metadata: artifact.Metadata.Clone(),
			ContentDigest: contentDigest(artifact),
		})
	}
	_, err = index.store.ApplyDelta(ctx, delta.Scope, index.storageProjection,
		projectionstore.EntryDelta[entry]{
			Upserts: upserts, DeleteIDs: delta.DeleteIDs,
			DeleteDocuments:    delta.DeleteDocuments,
			ReconcileDocuments: delta.ReconcileDocuments, ActiveIDs: delta.ActiveIDs,
		}, delta.SourceRevision, delta.SourceDigest)
	return err
}

func (index *Index) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	if err := validateSearch(request); err != nil {
		return nil, err
	}
	build, err := index.readSnapshot(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	if len(build.Entries) == 0 {
		return []component.Candidate{}, nil
	}
	content := requestContent(request.Query)
	response, err := index.runtime.Embed(ctx, index.model, inference.EmbedRequest{Items: []inference.EmbedItem{{Content: content}}})
	if err != nil {
		return nil, fmt.Errorf("vector projection: embed query: %w", err)
	}
	return searchSnapshot(build, response.Embeddings[0].Vector, component.VectorSearchFilter{
		Metadata: request.Metadata,
	}, request.Limit)
}

// SearchVector searches persisted projection vectors with a precomputed query
// vector and never invokes the inference runtime.
func (index *Index) SearchVector(ctx context.Context, request component.VectorSearchRequest) ([]component.Candidate, error) {
	if err := validateVectorSearch(request); err != nil {
		return nil, err
	}
	build, err := index.readSnapshot(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	return searchSnapshot(build, request.Vector, request.Filter, request.Limit)
}

func (index *Index) readSnapshot(ctx context.Context, scope sdkmemory.Scope) (snapshot, error) {
	if index == nil || index.store == nil {
		return snapshot{}, errors.New("vector projection: index is required")
	}
	build, _, err := index.store.Materialize(ctx, scope, index.storageProjection)
	if err != nil {
		return snapshot{}, fmt.Errorf("vector projection: %w", err)
	}
	return build, nil
}

func searchSnapshot(build snapshot, query []float32, filter component.VectorSearchFilter, limit int) ([]component.Candidate, error) {
	if len(build.Entries) == 0 {
		return []component.Candidate{}, nil
	}
	if len(query) != build.Dimensions {
		return nil, fmt.Errorf("vector projection: query dimension %d does not match index dimension %d", len(query), build.Dimensions)
	}
	queryNorm, err := norm(query)
	if err != nil {
		return nil, fmt.Errorf("vector projection: query: %w", err)
	}
	results := make([]component.Candidate, 0, len(build.Entries))
	for _, item := range build.Entries {
		if filter.Name != "" && item.Name != filter.Name {
			continue
		}
		matches, err := projectionstore.MatchesRequest(filter.Metadata, item.Address)
		if err != nil {
			return nil, fmt.Errorf("vector projection: selector: %w", err)
		}
		if !matches {
			continue
		}
		itemNorm, err := norm(item.Vector)
		if err != nil {
			return nil, fmt.Errorf("vector projection: entry %q: %w", item.ID, err)
		}
		var dot float64
		for i := range query {
			dot += float64(query[i]) * float64(item.Vector[i])
		}
		cosine := dot / (queryNorm * itemNorm)
		if math.IsNaN(cosine) || math.IsInf(cosine, 0) {
			return nil, fmt.Errorf("vector projection: entry %q produced non-finite cosine", item.ID)
		}
		cosine = max(-1, min(1, cosine))
		results = append(results, component.Candidate{
			ID: item.ID, Lane: laneName, Name: item.Name, Score: cosine,
			Source: item.Source, Address: item.Address, Metadata: item.Metadata.Clone(),
		})
	}
	sortCandidates(results)
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (index *Index) validateRequest(request component.ProjectionRequest) error {
	if index == nil || index.runtime == nil || index.store == nil {
		return errors.New("vector projection: index is required")
	}
	if request.Projection != "" && request.Projection != index.projection {
		return fmt.Errorf("vector projection: request projection %q does not match %q", request.Projection, index.projection)
	}
	return request.Scope.Validate()
}

func validateSearch(request component.SearchRequest) error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(request.Query) == "" {
		return errors.New("vector projection: query is required")
	}
	if request.Limit < 0 {
		return errors.New("vector projection: limit must not be negative")
	}
	return nil
}

func validateVectorSearch(request component.VectorSearchRequest) error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if request.Limit < 0 {
		return errors.New("vector projection: limit must not be negative")
	}
	if _, err := norm(request.Vector); err != nil {
		return fmt.Errorf("vector projection: query: %w", err)
	}
	return nil
}

func validateSnapshot(build snapshot) error {
	if len(build.Entries) == 0 {
		if build.Dimensions != 0 {
			return errors.New("empty index has non-zero dimensions")
		}
		return nil
	}
	if build.Dimensions <= 0 {
		return errors.New("dimensions must be positive")
	}
	seen := make(map[string]struct{}, len(build.Entries))
	for _, item := range build.Entries {
		if item.ID == "" || item.ContentDigest == "" || len(item.Vector) != build.Dimensions {
			return errors.New("entry identity or dimensions are invalid")
		}
		if _, ok := seen[item.ID]; ok {
			return fmt.Errorf("duplicate entry %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		if err := item.Source.Validate(); err != nil {
			return err
		}
		if err := item.Address.Validate(); err != nil {
			return err
		}
		if _, err := norm(item.Vector); err != nil {
			return err
		}
	}
	return nil
}

func recomputeDimensions(build *snapshot) error {
	build.Dimensions = 0
	for _, item := range build.Entries {
		if build.Dimensions == 0 {
			build.Dimensions = len(item.Vector)
		}
		if len(item.Vector) != build.Dimensions {
			return errors.New("vector projection: delta dimension mismatch")
		}
	}
	return nil
}

func vectorEntryKey() projectionstore.EntryKey[entry] {
	return projectionstore.EntryKey[entry]{
		ID: func(value entry) string { return value.ID },
		Document: func(value entry) component.DocumentAddress {
			return component.DocumentAddress{DatasetID: value.Address.DatasetID, DocumentID: value.Address.DocumentID}
		},
	}
}

func contentDigest(artifact component.Artifact) string {
	sum := sha256.Sum256([]byte(artifact.Content.Text()))
	return fmt.Sprintf("%x", sum[:])
}

// generationProjection isolates vectors produced by different embedding
// identities. A new model starts an empty generation and is filled by normal
// source replay; retrieval degrades through the other lanes until it exists.
func generationProjection(projection string, model inference.ModelRef) string {
	payload := strings.Join([]string{
		AlgorithmVersion, model.ID.Provider, model.ID.Name, model.Profile,
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%s.vector-%x", projection, sum[:12])
}

func digestArtifacts(artifacts []component.Artifact) string {
	entries := make([]entry, len(artifacts))
	for i, artifact := range artifacts {
		entries[i] = entry{ID: artifact.ID, ContentDigest: contentDigest(artifact)}
	}
	return digestEntries(entries)
}

func digestEntries(entries []entry) string {
	hash := sha256.New()
	ordered := append([]entry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, item := range ordered {
		_, _ = hash.Write([]byte(item.ID + "\x00" + item.ContentDigest + "\x00"))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func norm(vector []float32) (float64, error) {
	var squared float64
	for _, value := range vector {
		number := float64(value)
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, errors.New("vector contains non-finite value")
		}
		squared += number * number
	}
	if squared == 0 {
		return 0, errors.New("zero vector is not allowed")
	}
	return math.Sqrt(squared), nil
}

func sortCandidates(values []component.Candidate) {
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].Score == values[j].Score {
			return values[i].ID < values[j].ID
		}
		return values[i].Score > values[j].Score
	})
}

func requestContent(query string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: query}}}
}
