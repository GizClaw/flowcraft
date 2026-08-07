// Package entity implements an independent metadata-entity retrieval lane.
package entity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/memory/internal/projection"
	"github.com/GizClaw/flowcraft/memory/internal/textutil"
	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const (
	laneName         = "entity"
	AlgorithmVersion = "deterministic-entity-v1"
)

type Thresholds = projectionstore.Thresholds

type Config struct {
	KV         storage.Store
	Projection string
	Thresholds projectionstore.Thresholds
}

type Index struct {
	store      *projectionstore.Store[snapshot, projectionstore.EntryDelta[entry]]
	projection string
}

// AuditDigests independently recomputes and validates active projection digests.
func (index *Index) AuditDigests(
	ctx context.Context,
	scope sdkmemory.Scope,
) (string, string, string, string, bool, error) {
	evidence, found, err := index.store.AuditDigestEvidence(ctx, scope, index.projection)
	return evidence.StoredSourceDigest, evidence.ComputedSourceDigest,
		evidence.StoredBuildDigest, evidence.ComputedBuildDigest, found, err
}

type snapshot struct {
	Entries []entry `json:"entries"`
}

type entry struct {
	ID       string                     `json:"id"`
	Name     string                     `json:"name"`
	Entities []string                   `json:"entities"`
	Source   sdkmemory.SourceRef        `json:"source"`
	Address  component.CandidateAddress `json:"address"`
	Metadata sdkmemory.Metadata         `json:"metadata,omitempty"`
}

var _ component.Indexer = (*Index)(nil)
var _ component.Searcher = (*Index)(nil)
var _ component.DeltaIndexer = (*Index)(nil)
var _ component.FullRebuilder = (*Index)(nil)

func New(config Config) (*Index, error) {
	if strings.TrimSpace(config.Projection) == "" {
		return nil, errors.New("entity projection: projection name is required")
	}
	key := entityEntryKey()
	store, err := projectionstore.NewTypedStore(config.KV, laneName,
		projectionstore.TypedOptions[snapshot, projectionstore.EntryDelta[entry]]{
			Thresholds: config.Thresholds,
			Canonicalize: func(delta projectionstore.EntryDelta[entry]) projectionstore.EntryDelta[entry] {
				return projectionstore.CanonicalEntryDelta(delta, key)
			},
			ValidateBase: validateSnapshot,
			Apply: func(base *snapshot, delta projectionstore.EntryDelta[entry]) error {
				base.Entries = projectionstore.ApplyEntryDelta(base.Entries, delta, key)
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("entity projection: %w", err)
	}
	return &Index{store: store, projection: config.Projection}, nil
}

func (index *Index) Rebuild(ctx context.Context, request component.ProjectionRequest) error {
	return index.FullRebuild(ctx, request)
}

func (index *Index) FullRebuild(ctx context.Context, request component.ProjectionRequest) error {
	if err := index.validateRequest(request); err != nil {
		return err
	}
	entries := make([]entry, 0, len(request.Artifacts))
	for i, artifact := range request.Artifacts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("entity projection: artifact %d: %w", i, err)
		}
		entities := normalizeEntities(artifact.Entities)
		if len(entities) == 0 {
			var err error
			entities, err = parseEntities(artifact.Metadata["entities"])
			if err != nil {
				return fmt.Errorf("entity projection: artifact %q entities: %w", artifact.ID, err)
			}
		}
		if len(entities) == 0 {
			continue
		}
		address := component.AddressFromArtifact(artifact)
		if err := address.Validate(); err != nil {
			return fmt.Errorf("entity projection: artifact %q address: %w", artifact.ID, err)
		}
		entries = append(entries, entry{
			ID: artifact.ID, Name: string(artifact.Kind), Entities: entities,
			Source: artifact.Sources[0], Address: address, Metadata: artifact.Metadata.Clone(),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return index.store.FullRebuild(ctx, request.Scope, index.projection,
		snapshot{Entries: entries}, entriesDigest(entries))
}

func (index *Index) ApplyDelta(ctx context.Context, delta component.ProjectionDelta) error {
	if err := delta.ValidateDocumentAddresses(); err != nil {
		return fmt.Errorf("entity projection: %w", err)
	}
	if err := index.validateRequest(component.ProjectionRequest{Scope: delta.Scope, Projection: delta.Projection}); err != nil {
		return err
	}
	upserts := make([]entry, 0, len(delta.Upserts))
	deleteIDs := append([]string(nil), delta.DeleteIDs...)
	for _, artifact := range delta.Upserts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("entity projection: artifact %q: %w", artifact.ID, err)
		}
		entities := normalizeEntities(artifact.Entities)
		if len(entities) == 0 {
			var err error
			entities, err = parseEntities(artifact.Metadata["entities"])
			if err != nil {
				return fmt.Errorf("entity projection: artifact %q entities: %w", artifact.ID, err)
			}
		}
		if len(entities) == 0 {
			deleteIDs = append(deleteIDs, artifact.ID)
			continue
		}
		address := component.AddressFromArtifact(artifact)
		if err := address.Validate(); err != nil {
			return fmt.Errorf("entity projection: artifact %q address: %w", artifact.ID, err)
		}
		upserts = append(upserts, entry{
			ID: artifact.ID, Name: string(artifact.Kind), Entities: entities,
			Source: artifact.Sources[0], Address: address,
			Metadata: artifact.Metadata.Clone(),
		})
	}
	_, err := index.store.ApplyDelta(ctx, delta.Scope, index.projection,
		projectionstore.EntryDelta[entry]{
			Upserts: upserts, DeleteIDs: deleteIDs,
			DeleteDocuments:    delta.DeleteDocuments,
			ReconcileDocuments: delta.ReconcileDocuments, ActiveIDs: delta.ActiveIDs,
		}, delta.SourceRevision, delta.SourceDigest)
	return err
}

func (index *Index) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	if err := validateSearch(request); err != nil {
		return nil, err
	}
	build, _, err := index.store.Materialize(ctx, request.Scope, index.projection)
	if err != nil {
		return nil, fmt.Errorf("entity projection: %w", err)
	}
	queryEntities := extractEntities(request.Query, build.Entries)
	if len(queryEntities) == 0 {
		return []component.Candidate{}, nil
	}
	results := make([]component.Candidate, 0, len(build.Entries))
	for _, item := range build.Entries {
		matches, err := projectionstore.MatchesRequest(request.Metadata, item.Address)
		if err != nil {
			return nil, fmt.Errorf("entity projection: selector: %w", err)
		}
		if !matches {
			continue
		}
		score := entityScore(queryEntities, item.Entities)
		if score == 0 {
			continue
		}
		results = append(results, component.Candidate{
			ID: item.ID, Lane: laneName, Name: item.Name, Score: score,
			Source: item.Source, Address: item.Address, Metadata: item.Metadata.Clone(),
		})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].ID < results[j].ID
		}
		return results[i].Score > results[j].Score
	})
	if request.Limit > 0 && len(results) > request.Limit {
		results = results[:request.Limit]
	}
	return results, nil
}

func entriesDigest(entries []entry) string {
	hash := sha256.New()
	for _, item := range entries {
		_, _ = hash.Write([]byte(item.ID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(strings.Join(item.Entities, "\x00")))
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func parseEntities(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var values []string
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return nil, fmt.Errorf("decode JSON string array: %w", err)
		}
	} else {
		values = strings.Split(raw, ",")
	}
	return normalizeEntities(values), nil
}

func normalizeEntities(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		entity := strings.Join(textutil.Tokens(value), " ")
		if entity != "" {
			normalized = append(normalized, entity)
		}
	}
	normalized = textutil.Unique(normalized)
	sort.Strings(normalized)
	return normalized
}

// extractEntities is deterministic dictionary mention extraction over the
// active entity projection. Callers pass ordinary query text, never metadata.
func extractEntities(query string, entries []entry) []string {
	queryTokens := textutil.Tokens(query)
	mentioned := make(map[string]struct{})
	for _, item := range entries {
		for _, entity := range item.Entities {
			entityTokens := textutil.Tokens(entity)
			if containsTokens(queryTokens, entityTokens) {
				mentioned[entity] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(mentioned))
	for entity := range mentioned {
		result = append(result, entity)
	}
	sort.Strings(result)
	return result
}

func containsTokens(query, entity []string) bool {
	if len(entity) == 0 || len(entity) > len(query) {
		return false
	}
	for start := 0; start+len(entity) <= len(query); start++ {
		match := true
		for offset := range entity {
			if query[start+offset] != entity[offset] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// entityScore is the fraction of extracted query entities linked to the item.
func entityScore(queryEntities, entities []string) float64 {
	if len(queryEntities) == 0 {
		return 0
	}
	owned := make(map[string]struct{}, len(entities))
	for _, entity := range entities {
		owned[entity] = struct{}{}
	}
	matches := 0
	for _, entity := range queryEntities {
		if _, ok := owned[entity]; ok {
			matches++
		}
	}
	return float64(matches) / float64(len(queryEntities))
}

func (index *Index) validateRequest(request component.ProjectionRequest) error {
	if index == nil || index.store == nil {
		return errors.New("entity projection: index is required")
	}
	if request.Projection != "" && request.Projection != index.projection {
		return errors.New("entity projection: projection name mismatch")
	}
	return request.Scope.Validate()
}

func validateSearch(request component.SearchRequest) error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(request.Query) == "" {
		return errors.New("entity projection: query is required")
	}
	if request.Limit < 0 {
		return errors.New("entity projection: limit must not be negative")
	}
	return nil
}

func validateSnapshot(build snapshot) error {
	seen := make(map[string]struct{}, len(build.Entries))
	for _, item := range build.Entries {
		if item.ID == "" || len(item.Entities) == 0 {
			return errors.New("invalid entity entry")
		}
		if _, ok := seen[item.ID]; ok {
			return fmt.Errorf("duplicate entity entry %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		if err := item.Source.Validate(); err != nil {
			return err
		}
		if err := item.Address.Validate(); err != nil {
			return err
		}
		for _, value := range item.Entities {
			if value == "" {
				return errors.New("empty normalized entity")
			}
		}
	}
	return nil
}

func entityEntryKey() projectionstore.EntryKey[entry] {
	return projectionstore.EntryKey[entry]{
		ID: func(value entry) string { return value.ID },
		Document: func(value entry) component.DocumentAddress {
			return component.DocumentAddress{DatasetID: value.Address.DatasetID, DocumentID: value.Address.DocumentID}
		},
	}
}
