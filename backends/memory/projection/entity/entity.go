// Package entity implements an independent metadata-entity retrieval lane.
// Entity values are stored case-folded and whitespace-collapsed, and a mention
// is found by tokenizing the stored key and the query with the shared kernel
// (core/utils/bm25.Tokenize) and looking for the key's tokens as a contiguous
// run. That is what lets the lane match text with no whitespace word
// boundaries — a CJK entity inside a sentence — without owning a tokenizer of
// its own.
package entity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/errdefs"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	corebm25 "github.com/GizClaw/flowcraft/core/utils/bm25"
)

const (
	laneName = "entity"
	// AlgorithmVersion names the lane's algorithm identity. v3 matches with the
	// shared kernel's tokenizer instead of splitting on whitespace, so a CJK
	// entity is found inside a sentence. A key an older version stored still
	// matches when its token stream is a contiguous run of the query's, but it
	// counts as a vocabulary entry of its own and splits the IDF weight of the
	// same entity written both ways.
	AlgorithmVersion = "deterministic-entity-v3"

	// entitySaturationMinEntries and entitySaturationShare gate ubiquitous
	// entities out of query matching: once a corpus has at least
	// entitySaturationMinEntries entries, an entity that appears in
	// entitySaturationShare or more of them carries no selectivity and only
	// floods the lane with rank votes.
	entitySaturationMinEntries = 8
	entitySaturationShare      = 0.9
	// entityQueryLimit caps how many query entities one search selects.
	entityQueryLimit = 8
)

type Thresholds = projectionstore.Thresholds

type Config struct {
	KV         storage.Store
	Projection string
	Thresholds projectionstore.Thresholds
	// CacheBytes bounds the tables the store behind this lane keeps decoded.
	// Zero uses the store's default budget.
	CacheBytes int64
}

type Index struct {
	store      *projectionstore.Store[snapshot, projectionstore.EntryDelta[entry]]
	projection string
	// producerVersion is the identity of the state this index writes. The
	// entity lane re-tokenizes stored keys on every search, so its algorithm
	// version is the whole identity: it has no persisted scoring parameter a
	// build carries the way the bm25 lane does.
	producerVersion string
}

// AuditDigests independently recomputes and validates active projection digests.
func (index *Index) AuditDigests(
	ctx context.Context,
	scope corememory.Scope,
) (string, string, string, string, bool, error) {
	evidence, found, err := index.store.AuditDigestEvidence(ctx, scope, index.projection)
	return evidence.StoredSourceDigest, evidence.ComputedSourceDigest,
		evidence.StoredBuildDigest, evidence.ComputedBuildDigest, found, err
}

type snapshot struct {
	Entries []entry `json:"entries"`
}

type entry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Entities holds normalized match keys, not the raw metadata values:
	// case-folded, whitespace-collapsed, deduplicated and sorted. The keys stay
	// text rather than tokens, so the match can tokenize both the stored key
	// and the query back into the run they came from.
	Entities []string                   `json:"entities"`
	Source   corememory.SourceRef       `json:"source"`
	Address  component.CandidateAddress `json:"address"`
	Metadata corememory.Metadata        `json:"metadata,omitempty"`
}

var _ component.Indexer = (*Index)(nil)
var _ component.Searcher = (*Index)(nil)
var _ component.DeltaIndexer = (*Index)(nil)
var _ component.FullRebuilder = (*Index)(nil)
var _ component.Producer = (*Index)(nil)
var _ component.Reclaimer = (*Index)(nil)

func New(config Config) (*Index, error) {
	if strings.TrimSpace(config.Projection) == "" {
		return nil, errors.New("entity projection: projection name is required")
	}
	key := entityEntryKey()
	store, err := projectionstore.NewTypedStore(config.KV, laneName,
		projectionstore.TypedOptions[snapshot, projectionstore.EntryDelta[entry]]{
			ProducerVersion: AlgorithmVersion,
			Thresholds:      config.Thresholds,
			MaxBytes:        config.CacheBytes,
			BaseBytes:       tableBytes,
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
	return &Index{store: store, projection: config.Projection, producerVersion: AlgorithmVersion}, nil
}

func (index *Index) Rebuild(ctx context.Context, request component.ProjectionRequest) error {
	return index.FullRebuild(ctx, request)
}

// ProducerVersion names the identity of the state this lane writes.
func (index *Index) ProducerVersion() string { return index.producerVersion }

// StoredProducerIdentity reports the identity of the lane's persisted state for
// one scope; found is false while the lane holds no state for it.
func (index *Index) StoredProducerIdentity(
	ctx context.Context,
	scope corememory.Scope,
) (component.ProducerIdentity, bool, error) {
	return index.store.ProducerIdentity(ctx, scope, index.projection)
}

// OtherStoredNames reports the storage names this lane's family holds state
// under for one scope, excluding the name this lane writes. The entity lane
// keeps one name, so the list is what a deployment that changed its projection
// name left behind.
func (index *Index) OtherStoredNames(ctx context.Context, scope corememory.Scope) ([]string, error) {
	return index.store.OtherNames(ctx, scope, index.projection)
}

// ReclaimSuperseded removes the bases and segments the lane compacted or
// rebuilt away, keeping the one the active manifest names. The entity lane
// names its storage after the projection setting alone, so the sweep is exactly
// the lane's own area: no generation of this family is something the lane is
// not.
func (index *Index) ReclaimSuperseded(
	ctx context.Context,
	scope corememory.Scope,
) (component.ReclaimResult, error) {
	return index.store.Reclaim(ctx, scope, index.projection)
}

func (index *Index) FullRebuild(ctx context.Context, request component.ProjectionRequest) error {
	if err := index.validateRequest(request); err != nil {
		return err
	}
	groups, err := component.GroupArtifactsByShard(request.Artifacts)
	if err != nil {
		return fmt.Errorf("entity projection: %w", err)
	}
	covered := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		entries, err := index.buildEntries(group.Artifacts)
		if err != nil {
			return err
		}
		if err := index.store.FullRebuild(ctx,
			projectionstore.Address{Scope: request.Scope, Shard: group.Shard}, index.projection,
			snapshot{Entries: entries}, entriesDigest(entries)); err != nil {
			return err
		}
		covered[group.Shard] = struct{}{}
	}
	return index.dropUncoveredShards(ctx, request.Scope, covered)
}

// buildEntries turns one shard's artifacts into its entries, sorted by ID.
func (index *Index) buildEntries(artifacts []component.Artifact) ([]entry, error) {
	entries := make([]entry, 0, len(artifacts))
	for i, artifact := range artifacts {
		if err := artifact.Validate(); err != nil {
			return nil, fmt.Errorf("entity projection: artifact %d: %w", i, err)
		}
		entities := normalizeEntities(artifact.Entities)
		if len(entities) == 0 {
			var err error
			entities, err = parseEntities(artifact.Metadata["entities"])
			if err != nil {
				return nil, fmt.Errorf("entity projection: artifact %q entities: %w", artifact.ID, err)
			}
		}
		if len(entities) == 0 {
			continue
		}
		address := component.AddressFromArtifact(artifact)
		if err := address.Validate(); err != nil {
			return nil, fmt.Errorf("entity projection: artifact %q address: %w", artifact.ID, err)
		}
		entries = append(entries, entry{
			ID: artifact.ID, Name: string(artifact.Kind), Entities: entities,
			Source: artifact.Sources[0], Address: address, Metadata: artifact.Metadata.Clone(),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

// dropUncoveredShards removes the shards a scope-wide rebuild did not publish.
func (index *Index) dropUncoveredShards(
	ctx context.Context,
	scope corememory.Scope,
	covered map[string]struct{},
) error {
	if _, err := index.store.PruneShards(ctx, scope, index.projection, covered); err != nil {
		return fmt.Errorf("entity projection: %w", err)
	}
	return nil
}

func (index *Index) ApplyDelta(ctx context.Context, delta component.ProjectionDelta) error {
	if err := delta.ValidateDocumentAddresses(); err != nil {
		return fmt.Errorf("entity projection: %w", err)
	}
	if err := index.validateRequest(component.ProjectionRequest{Scope: delta.Scope, Projection: delta.Projection}); err != nil {
		return err
	}
	if err := delta.ValidateShard(); err != nil {
		return fmt.Errorf("entity projection: %w", err)
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
	_, err := index.store.ApplyDelta(ctx,
		projectionstore.Address{Scope: delta.Scope, Shard: delta.Shard}, index.projection,
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
	selector, err := projectionstore.ParseSelector(request.Metadata)
	if err != nil {
		return nil, fmt.Errorf("entity projection: selector: %w", err)
	}
	shards, err := index.searchShards(ctx, request.Scope, selector)
	if err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return []component.Candidate{}, nil
	}
	// Pass 1 accumulates the scoring set's statistics: how many records the
	// selector admits and how many of them link each entity. Both are
	// properties of the set, not of one shard, so they have to be counted
	// before any record is scored -- a shard scored against its own counts
	// would weigh the same entity differently from the shard next to it, and
	// the scores could no longer be compared.
	documents := 0
	frequency := make(map[string]int)
	for _, shard := range shards {
		build, found, err := index.materialize(ctx, request.Scope, shard)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		for _, item := range build.Entries {
			if !selector.Matches(item.Address) {
				continue
			}
			documents++
			seen := make(map[string]struct{}, len(item.Entities))
			for _, entity := range item.Entities {
				if _, duplicate := seen[entity]; duplicate {
					continue
				}
				seen[entity] = struct{}{}
				frequency[entity]++
			}
		}
	}
	queryEntities, idf := queryEntitySelection(request.Query, documents, frequency)
	if len(queryEntities) == 0 {
		return []component.Candidate{}, nil
	}
	// Pass 2 scores the same shards against the statistics pass 1 settled, and
	// each shard contributes its own best records; the merge keeps the top
	// limit of the union under the lane's order, which is total, so the result
	// is the one a single table holding every planned shard would produce.
	collected := make([][]component.Candidate, 0, len(shards))
	for _, shard := range shards {
		build, found, err := index.materialize(ctx, request.Scope, shard)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		results := make([]component.Candidate, 0, len(build.Entries))
		for _, item := range build.Entries {
			if !selector.Matches(item.Address) {
				continue
			}
			score := entityScore(queryEntities, idf, item.Entities)
			if score == 0 {
				continue
			}
			results = append(results, component.Candidate{
				ID: item.ID, Lane: laneName, Name: item.Name, Score: score,
				Source: item.Source, Address: item.Address, Metadata: item.Metadata.Clone(),
			})
		}
		sort.Slice(results, func(i, j int) bool {
			if results[i].Score == results[j].Score {
				return results[i].ID < results[j].ID
			}
			return results[i].Score > results[j].Score
		})
		if request.Limit > 0 && len(results) > request.Limit {
			results = results[:request.Limit]
		}
		if len(results) > 0 {
			collected = append(collected, results)
		}
	}
	return component.MergeCandidates(collected, request.Limit), nil
}

// searchShards resolves the shards one read has to touch: the ones the selector
// names, or every shard the lane holds when the request narrows nothing.
func (index *Index) searchShards(
	ctx context.Context,
	scope corememory.Scope,
	selector projectionstore.Selector,
) ([]string, error) {
	plan, err := selector.Plan()
	if err != nil {
		return nil, fmt.Errorf("entity projection: selector: %w", err)
	}
	if !plan.All {
		return plan.Shards, nil
	}
	shards, err := index.store.Shards(ctx, scope, index.projection)
	if err != nil {
		return nil, fmt.Errorf("entity projection: %w", err)
	}
	return shards, nil
}

// materialize reads one shard's table. A shard the plan names that was never
// written holds nothing, which is an empty read rather than a failed one.
func (index *Index) materialize(ctx context.Context, scope corememory.Scope, shard string) (snapshot, bool, error) {
	build, _, err := index.store.MaterializeView(ctx,
		projectionstore.Address{Scope: scope, Shard: shard}, index.projection)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return snapshot{}, false, nil
		}
		return snapshot{}, false, fmt.Errorf("entity projection: %w", err)
	}
	return build, true, nil
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
		entity := normalizeEntityText(value)
		// A value that tokenizes to nothing — empty text, or punctuation
		// only — can never match a query, so it is dropped instead of
		// being kept as a vocabulary entry that only raises the IDF
		// denominator and the ubiquity gate. An artifact left with no
		// entity at all is a delete on the delta path, as before.
		if entity == "" || len(corebm25.Tokenize(entity)) == 0 {
			continue
		}
		normalized = append(normalized, entity)
	}
	normalized = uniqueStrings(normalized)
	sort.Strings(normalized)
	return normalized
}

// normalizeEntityText case folds one entity value and collapses its whitespace.
// Punctuation stays: the match compares token streams, which drop it anyway,
// while keeping it leaves two spellings distinguishable in the vocabulary the
// IDF weights are counted over (a value that is nothing but punctuation has no
// token stream at all and normalizeEntities drops it). The key must stay the
// text and not the tokens, because a token stream is not reversible —
// re-tokenizing an entity's bigram would emit its characters a second time —
// and the match needs to tokenize the stored key back into the run the query is
// searched for.
func normalizeEntityText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

// uniqueStrings keeps each distinct non-empty value once, in first-appearance
// order, so two raw values that normalize to the same entity stay one entry.
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// queryEntitySelection matches the query against the entity vocabulary and
// returns the most selective entities together with their IDF weights.
// Ubiquitous entities are gated out once the corpus is large enough, and the
// selection is capped so a long query cannot vote with dozens of entities. A
// sharded read counts the scoring set's frequency across shards first and then
// selects, so the weights it applies are the set's and not one shard's;
// documents and frequency must describe the same records.
func queryEntitySelection(query string, documents int, frequency map[string]int) ([]string, map[string]float64) {
	queryTokens := corebm25.Tokenize(query)
	if documents == 0 || len(queryTokens) == 0 {
		return nil, nil
	}
	mentioned := make(map[string]struct{})
	idf := make(map[string]float64, len(frequency))
	for entity, count := range frequency {
		entityTokens := corebm25.Tokenize(entity)
		if !containsTokens(queryTokens, entityTokens) {
			continue
		}
		if documents >= entitySaturationMinEntries &&
			float64(count) >= entitySaturationShare*float64(documents) {
			continue
		}
		mentioned[entity] = struct{}{}
		idf[entity] = math.Log(1 + float64(documents)/float64(count))
	}
	result := make([]string, 0, len(mentioned))
	for entity := range mentioned {
		result = append(result, entity)
	}
	sort.Slice(result, func(i, j int) bool {
		if idf[result[i]] == idf[result[j]] {
			return result[i] < result[j]
		}
		return idf[result[i]] > idf[result[j]]
	})
	if len(result) > entityQueryLimit {
		result = result[:entityQueryLimit]
	}
	return result, idf
}

// containsTokens reports whether the query's token stream contains the
// entity's tokens as a contiguous run. Both sides are kernel tokens, so a CJK
// entity matches a query that mentions it even though the two share no word
// boundary to split on.
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

// entityScore is the IDF-weighted share of the selected query entities that
// the item links: a rare entity match outweighs a common one.
func entityScore(queryEntities []string, idf map[string]float64, entities []string) float64 {
	if len(queryEntities) == 0 {
		return 0
	}
	owned := make(map[string]struct{}, len(entities))
	for _, entity := range entities {
		owned[entity] = struct{}{}
	}
	total := 0.0
	matched := 0.0
	for _, entity := range queryEntities {
		weight := idf[entity]
		if weight <= 0 {
			weight = 1
		}
		total += weight
		if _, ok := owned[entity]; ok {
			matched += weight
		}
	}
	if total == 0 {
		return 0
	}
	return matched / total
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

const (
	// entityKeyBytes is what one normalized entity key costs beyond its text:
	// the slice slot, the string header, and the entry it sits in.
	entityKeyBytes = 32
	// entryBytes is what one entry costs beyond its keys: its id, its source
	// reference, and its address.
	entryBytes = 200
)

// tableBytes weighs one table for the store's cache budget: the normalized keys
// the lane matches against, which is what makes one table larger than another,
// plus a fixed cost per entry for the identity around them. The constants are
// estimates of the runtime's own overhead, not a measurement of it.
func tableBytes(build snapshot) int64 {
	weight := int64(0)
	for _, item := range build.Entries {
		weight += entryBytes
		for _, value := range item.Entities {
			weight += int64(len(value)) + entityKeyBytes
		}
	}
	return weight
}

func entityEntryKey() projectionstore.EntryKey[entry] {
	return projectionstore.EntryKey[entry]{
		ID: func(value entry) string { return value.ID },
		Document: func(value entry) component.DocumentAddress {
			return component.DocumentAddress{DatasetID: value.Address.DatasetID, DocumentID: value.Address.DocumentID}
		},
	}
}
