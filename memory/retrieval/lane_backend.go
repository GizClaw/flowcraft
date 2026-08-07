package retrieval

import (
	"context"
	"errors"
	"sort"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

// LaneBackend adapts one in-process component.Searcher lane (plus optional
// write lanes) to SearchBackend. It deliberately keeps the component.*
// plugin contract intact: built-in lanes and custom registry searchers are
// wrapped, never replaced.
//
// Scores are lane-native: the lsm driver relies on the upper fusion layer's
// per-lane calibrators, so it does not pre-normalize. This is a documented
// refinement of the generic normalization obligation, which applies to
// backends that compute distances directly.
type LaneBackend struct {
	searcher component.Searcher
	indexer  component.Indexer
	delta    component.DeltaIndexer
	full     component.FullRebuilder
}

var _ SearchBackend = (*LaneBackend)(nil)

// NewLaneBackend wraps one searcher lane. The same lane may also implement
// component.Indexer / DeltaIndexer / FullRebuilder; if it does, the
// corresponding SearchBackend write operations are available.
func NewLaneBackend(searcher component.Searcher) *LaneBackend {
	backend := &LaneBackend{searcher: searcher}
	if indexer, ok := searcher.(component.Indexer); ok {
		backend.indexer = indexer
	}
	if delta, ok := searcher.(component.DeltaIndexer); ok {
		backend.delta = delta
	}
	if full, ok := searcher.(component.FullRebuilder); ok {
		backend.full = full
	}
	return backend
}

// Search implements SearchBackend.
func (backend *LaneBackend) Search(ctx context.Context, index string, query SearchQuery) ([]Hit, error) {
	if backend == nil || backend.searcher == nil {
		return nil, errors.New("retrieval: lane backend is incomplete")
	}
	request := component.SearchRequest{
		Scope:    query.Scope,
		Query:    query.Text,
		Limit:    query.TopK,
		Metadata: filterMetadata(query.Filters),
	}
	candidates, err := backend.searcher.Search(ctx, request)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(candidates))
	for _, candidate := range candidates {
		if query.Threshold > 0 && candidate.Score < query.Threshold {
			continue
		}
		hits = append(hits, Hit{
			ID:    candidate.ID,
			Score: candidate.Score,
			Payload: map[string]any{
				"address":  candidate.Address,
				"source":   candidate.Source,
				"lane":     candidate.Lane,
				"metadata": candidate.Metadata,
			},
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if query.TopK > 0 && len(hits) > query.TopK {
		hits = hits[:query.TopK]
	}
	return hits, nil
}

// Upsert implements SearchBackend.
func (backend *LaneBackend) Upsert(ctx context.Context, index string, scope sdkmemory.Scope, id string, doc Document) error {
	if backend == nil || backend.delta == nil {
		return errors.New("retrieval: lane backend has no delta writer")
	}
	kind, err := payloadKind(doc.Payload)
	if err != nil {
		return err
	}
	artifact := component.Artifact{
		Kind:     kind,
		ID:       id,
		Content:  textContent(doc.Text),
		Sources:  sourcesFromPayload(doc.Payload),
		Metadata: metadataFromPayload(doc.Payload),
	}
	err = backend.delta.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: scope, Projection: index, Upserts: []component.Artifact{artifact}, SourceRevision: id,
	})
	return err
}

// Delete implements SearchBackend.
func (backend *LaneBackend) Delete(ctx context.Context, index string, scope sdkmemory.Scope, id string) error {
	if backend == nil || backend.delta == nil {
		return errors.New("retrieval: lane backend has no delta writer")
	}
	return backend.delta.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: scope, Projection: index, DeleteIDs: []string{id}, SourceRevision: id,
	})
}

// ReplaceAll implements SearchBackend.
func (backend *LaneBackend) ReplaceAll(ctx context.Context, index string, scope sdkmemory.Scope, docs []Document) error {
	if backend == nil || backend.full == nil {
		return errors.New("retrieval: lane backend has no full rebuilder")
	}
	artifacts := make([]component.Artifact, 0, len(docs))
	for _, doc := range docs {
		kind, err := payloadKind(doc.Payload)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, component.Artifact{
			Kind:     kind,
			ID:       doc.ID,
			Content:  textContent(doc.Text),
			Sources:  sourcesFromPayload(doc.Payload),
			Metadata: metadataFromPayload(doc.Payload),
		})
	}
	if err := backend.full.FullRebuild(ctx, component.ProjectionRequest{
		Scope: scope, Projection: index, Artifacts: artifacts,
	}); err != nil {
		return err
	}
	return nil
}

func filterMetadata(filters map[string]any) sdkmemory.Metadata {
	metadata := sdkmemory.Metadata{}
	for key, value := range filters {
		if text, ok := value.(string); ok {
			metadata[key] = text
		}
	}
	return metadata
}

func textContent(text string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}}
}

func sourcesFromPayload(payload map[string]any) []sdkmemory.SourceRef {
	raw, ok := payload["sources"].([]sdkmemory.SourceRef)
	if !ok {
		return nil
	}
	return raw
}

func metadataFromPayload(payload map[string]any) sdkmemory.Metadata {
	raw, ok := payload["metadata"].(sdkmemory.Metadata)
	if !ok {
		return sdkmemory.Metadata{}
	}
	return raw
}

func payloadKind(payload map[string]any) (component.ArtifactKind, error) {
	raw, ok := payload["kind"].(string)
	if !ok || raw == "" {
		return "", errors.New("retrieval: document payload must carry an artifact kind")
	}
	return component.ArtifactKind(raw), nil
}
