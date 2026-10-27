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
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

const (
	laneName         = "vector"
	AlgorithmVersion = "vector-cosine-v2"
	// EncodingVersion names the form of one persisted vector payload. It is
	// part of the generation a lane writes -- the name its state lives under
	// and the stamp its manifest carries -- so changing the payload form moves
	// the lane to a generation nothing was written under, instead of
	// reinterpreting the payloads an earlier build published as if they were
	// this build's.
	EncodingVersion         = "vector-base64-v1"
	StorageAlgorithmVersion = projectionstore.StorageAlgorithmVersion
)

// Embedder is the narrow inference surface the vector projection needs.
// *inference.Assembly satisfies it.
type Embedder interface {
	Embed(context.Context, model.ModelRef, inference.EmbedRequest) (inference.EmbedResponse, error)
}

type Thresholds = projectionstore.Thresholds

func DefaultThresholds() Thresholds { return projectionstore.DefaultThresholds() }

type Config struct {
	KV         storage.Store
	Runtime    Embedder
	Model      model.ModelRef
	Projection string
	Thresholds projectionstore.Thresholds
	// CacheBytes bounds the tables the store behind this lane keeps decoded.
	// Zero uses the store's default budget.
	CacheBytes int64
}

type Index struct {
	store             *projectionstore.Store[snapshot, projectionstore.EntryDelta[entry]]
	runtime           Embedder
	model             model.ModelRef
	projection        string
	storageProjection string
	// producerVersion is the identity of the state this index writes. The
	// scoring algorithm and the model identity already name the storage
	// projection a generation is written to, so a stored version can only
	// match its own generation's; the stamp records which producer wrote it.
	producerVersion string
	// stats accumulates the phase counters Stats() publishes. It is written on
	// the serving path with atomics only, so no reader can perturb what the
	// lane serves.
	stats *statsState
}

// AuditDigests independently recomputes and validates active projection digests.
func (index *Index) AuditDigests(
	ctx context.Context,
	scope corememory.Scope,
) (string, string, string, string, bool, error) {
	evidence, found, err := index.store.AuditDigestEvidence(ctx, scope, index.storageProjection)
	return evidence.StoredSourceDigest, evidence.ComputedSourceDigest,
		evidence.StoredBuildDigest, evidence.ComputedBuildDigest, found, err
}

type snapshot struct {
	Dimensions int     `json:"dimensions"`
	Entries    []entry `json:"entries"`
	// norms holds one L2 norm per entry, in the same order as Entries: the
	// vector's own length, which the cosine needs and which the entries
	// already determine. It is derived, so it is never persisted -- the
	// stored state is the entries themselves, and a norm written next to them
	// would change every digest that names this state without saying anything
	// the vectors do not. A table read off the store therefore carries none
	// until the lane fills them in, and a table without them is scored the
	// old way: they are an optimization, not a requirement.
	norms []float64
}

type entry struct {
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Vector        Vector                     `json:"vector"`
	Source        corememory.SourceRef       `json:"source"`
	Address       component.CandidateAddress `json:"address"`
	Metadata      corememory.Metadata        `json:"metadata,omitempty"`
	ContentDigest string                     `json:"content_digest"`
}

var _ component.Indexer = (*Index)(nil)
var _ component.DeltaIndexer = (*Index)(nil)
var _ component.FullRebuilder = (*Index)(nil)
var _ component.Searcher = (*Index)(nil)
var _ component.VectorSearcher = (*Index)(nil)
var _ component.Producer = (*Index)(nil)
var _ component.Reclaimer = (*Index)(nil)

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
	store, err := projectionstore.NewTypedStore(config.KV, laneName,
		projectionstore.TypedOptions[snapshot, projectionstore.EntryDelta[entry]]{
			ProducerVersion: generationIdentity(config.Model),
			Thresholds:      config.Thresholds,
			MaxBytes:        config.CacheBytes,
			BaseBytes:       tableBytes,
			Canonicalize: func(delta projectionstore.EntryDelta[entry]) projectionstore.EntryDelta[entry] {
				return projectionstore.CanonicalEntryDelta(delta, key)
			},
			ValidateBase: validateSnapshot,
			Apply: func(base *snapshot, delta projectionstore.EntryDelta[entry]) error {
				base.Entries = projectionstore.ApplyEntryDelta(base.Entries, delta, key)
				if err := recomputeDimensions(base); err != nil {
					return err
				}
				// The norms are derived from the entries the delta replaced,
				// and a kept base is advanced by this call: a norm of the
				// table that was there is not a norm of the one that is.
				base.norms = nil
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("vector projection: %w", err)
	}
	return &Index{
		store: store, runtime: config.Runtime, model: config.Model, projection: config.Projection,
		storageProjection: generationProjection(config.Projection, config.Model),
		producerVersion:   generationIdentity(config.Model),
		stats:             &statsState{},
	}, nil
}

func (index *Index) Rebuild(ctx context.Context, request component.ProjectionRequest) error {
	return index.FullRebuild(ctx, request)
}

// ProducerVersion names the identity of the state this lane writes.
func (index *Index) ProducerVersion() string { return index.producerVersion }

// StoredProducerIdentity reports the identity of the lane's persisted state for
// one scope; found is false while no generation for the configured model holds
// state for it.
func (index *Index) StoredProducerIdentity(
	ctx context.Context,
	scope corememory.Scope,
) (component.ProducerIdentity, bool, error) {
	return index.store.ProducerIdentity(ctx, scope, index.storageProjection)
}

// OtherStoredNames reports the storage names this lane's family holds state
// under for one scope, excluding the generation this lane writes. A generation
// is named after the algorithm and the embedding model that produced it, so a
// new generation starts empty and leaves the previous one on disk under a name
// this lane no longer reads; that is what this reports.
func (index *Index) OtherStoredNames(ctx context.Context, scope corememory.Scope) ([]string, error) {
	return index.store.OtherNames(ctx, scope, index.storageProjection)
}

// ReclaimSuperseded removes the bases and segments the lane compacted or
// rebuilt away, keeping the one the active manifest names.
//
// It sweeps the generation this lane reads and no other. A generation another
// producer filled is a name a deployment still on that producer serves, and the
// state a version bump left behind is the state a rollback back to it reads;
// `OtherStoredNames` is what reports it, and repairing the lane is what fills
// the name this run reads. Reclaiming it here would make a rebuild delete
// another deployment's index.
func (index *Index) ReclaimSuperseded(
	ctx context.Context,
	scope corememory.Scope,
) (component.ReclaimResult, error) {
	return index.store.Reclaim(ctx, scope, index.storageProjection)
}

func (index *Index) FullRebuild(ctx context.Context, request component.ProjectionRequest) error {
	if err := index.validateRequest(request); err != nil {
		return err
	}
	groups, err := component.GroupArtifactsByShard(request.Artifacts)
	if err != nil {
		return fmt.Errorf("vector projection: %w", err)
	}
	covered := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		address := projectionstore.Address{Scope: request.Scope, Shard: group.Shard}
		build, err := index.embedArtifacts(ctx, group.Artifacts)
		if err != nil {
			return err
		}
		if err := index.store.FullRebuild(ctx, address, index.storageProjection, build,
			digestArtifacts(group.Artifacts)); err != nil {
			return err
		}
		covered[group.Shard] = struct{}{}
	}
	// A rebuild replaces the lane for the scope, so a shard the artifact set no
	// longer covers is state the canonical log no longer has: it has to stop
	// being readable, or a deleted conversation keeps answering queries.
	return index.dropUncoveredShards(ctx, request.Scope, covered)
}

// embedArtifacts builds one shard's table from its artifacts: one embed call
// per shard, so a rebuild of a scope with many conversations does not need the
// whole scope's text in one request, and a failed shard leaves the shards
// already published untouched instead of discarding a scope-sized batch.
func (index *Index) embedArtifacts(ctx context.Context, artifacts []component.Artifact) (snapshot, error) {
	if len(artifacts) == 0 {
		return snapshot{}, nil
	}
	items := make([]inference.EmbedItem, len(artifacts))
	for i, artifact := range artifacts {
		if err := artifact.Validate(); err != nil {
			return snapshot{}, fmt.Errorf("vector projection: artifact %d: %w", i, err)
		}
		items[i] = inference.EmbedItem{Content: sanitizeEmbedContent(artifact.Content)}
	}
	response, err := index.embedWithTextFallback(ctx, items)
	if err != nil {
		return snapshot{}, fmt.Errorf("vector projection: embed artifacts: %w", err)
	}
	entries := make([]entry, len(artifacts))
	dimensions := len(response.Embeddings[0].Vector)
	for i, artifact := range artifacts {
		vector := append([]float32(nil), response.Embeddings[i].Vector...)
		if _, err := norm(vector); err != nil {
			return snapshot{}, fmt.Errorf("vector projection: artifact %q: %w", artifact.ID, err)
		}
		address := component.AddressFromArtifact(artifact)
		if err := address.Validate(); err != nil {
			return snapshot{}, fmt.Errorf("vector projection: artifact %q address: %w", artifact.ID, err)
		}
		entries[i] = entry{
			ID: artifact.ID, Name: string(artifact.Kind), Vector: vector,
			Source: artifact.Sources[0], Address: address,
			Metadata:      artifact.Metadata.Clone(),
			ContentDigest: contentDigest(artifact),
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return snapshot{Dimensions: dimensions, Entries: entries}, nil
}

// dropUncoveredShards removes the shards a scope-wide rebuild did not publish.
func (index *Index) dropUncoveredShards(
	ctx context.Context,
	scope corememory.Scope,
	covered map[string]struct{},
) error {
	if _, err := index.store.PruneShards(ctx, scope, index.storageProjection, covered); err != nil {
		return fmt.Errorf("vector projection: %w", err)
	}
	return nil
}

func (index *Index) ApplyDelta(ctx context.Context, delta component.ProjectionDelta) error {
	address := projectionstore.Address{Scope: delta.Scope, Shard: delta.Shard}
	if err := delta.ValidateDocumentAddresses(); err != nil {
		return fmt.Errorf("vector projection: %w", err)
	}
	if err := delta.ValidateShard(); err != nil {
		return fmt.Errorf("vector projection: %w", err)
	}
	request := component.ProjectionRequest{Scope: delta.Scope, Projection: delta.Projection}
	if err := index.validateRequest(request); err != nil {
		return err
	}
	// The base is read, not taken: the delta needs the entries it already
	// holds -- to embed only what changed, and to re-use the vector of what did
	// not -- and nothing here writes to them. Copying a scope-sized table per
	// commit is what made the derivation quadratic.
	build, _, err := index.store.MaterializeView(ctx, address, index.storageProjection)
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
			items[i] = inference.EmbedItem{Content: sanitizeEmbedContent(artifact.Content)}
		}
		response, err := index.embedWithTextFallback(ctx, items)
		if err != nil {
			return fmt.Errorf("vector projection: embed delta: %w", err)
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
	_, err = index.store.ApplyDelta(ctx, address, index.storageProjection,
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
	index.recordQuery()
	shards, err := index.searchShards(ctx, request.Scope, request.Metadata)
	if err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return []component.Candidate{}, nil
	}
	content := requestContent(request.Query)
	embedStarted := time.Now()
	response, err := index.runtime.Embed(ctx, index.model, inference.EmbedRequest{Items: []inference.EmbedItem{{Content: content}}})
	index.recordEmbed(time.Since(embedStarted))
	if err != nil {
		return nil, fmt.Errorf("vector projection: embed query: %w", err)
	}
	return index.scanShards(ctx, shards, response.Embeddings[0].Vector,
		request.Scope, component.VectorSearchFilter{Metadata: request.Metadata}, request.Limit)
}

// SearchVector searches persisted projection vectors with a precomputed query
// vector and never invokes the inference runtime.
func (index *Index) SearchVector(ctx context.Context, request component.VectorSearchRequest) ([]component.Candidate, error) {
	if err := validateVectorSearch(request); err != nil {
		return nil, err
	}
	index.recordQuery()
	shards, err := index.searchShards(ctx, request.Scope, request.Filter.Metadata)
	if err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return []component.Candidate{}, nil
	}
	return index.scanShards(ctx, shards, request.Vector, request.Scope, request.Filter, request.Limit)
}

// searchShards resolves the shards one read has to touch. A request that names
// an axis reads the shards it names; a request that names nothing is the exact
// whole-scope recall, and enumerates every shard the lane holds.
func (index *Index) searchShards(
	ctx context.Context,
	scope corememory.Scope,
	metadata corememory.Metadata,
) ([]string, error) {
	plan, err := projectionstore.ShardsFor(metadata)
	if err != nil {
		return nil, fmt.Errorf("vector projection: selector: %w", err)
	}
	if !plan.All {
		return plan.Shards, nil
	}
	shards, err := index.store.Shards(ctx, scope, index.storageProjection)
	if err != nil {
		return nil, fmt.Errorf("vector projection: %w", err)
	}
	return shards, nil
}

// scanShards scores every planned shard and merges the results. Each shard is
// read on its own, and the merge keeps the top limit of the union under the
// order the scan publishes -- higher score, then lower id -- which is a total
// order, so the result is the same as scanning one table that held every
// planned shard.
func (index *Index) scanShards(
	ctx context.Context,
	shards []string,
	query []float32,
	scope corememory.Scope,
	filter component.VectorSearchFilter,
	limit int,
) ([]component.Candidate, error) {
	var report scanReport
	collected := make([][]component.Candidate, 0, len(shards))
	for _, shard := range shards {
		address := projectionstore.Address{Scope: scope, Shard: shard}
		loadStarted := time.Now()
		build, err := index.readSnapshot(ctx, address)
		index.recordShardLoad(time.Since(loadStarted), err != nil)
		if err != nil {
			if errdefs.IsNotFound(err) {
				// A shard the plan names that was never written holds
				// nothing: that is an empty read, not a failed one.
				continue
			}
			return nil, err
		}
		if len(build.Entries) == 0 {
			continue
		}
		results, shardReport, err := searchSnapshot(build, query, filter, limit)
		if err != nil {
			return nil, err
		}
		report.scanned += shardReport.scanned
		report.scored += shardReport.scored
		report.scanNanos += shardReport.scanNanos
		report.scoreNanos += shardReport.scoreNanos
		report.sortNanos += shardReport.sortNanos
		collected = append(collected, results)
	}
	index.recordScan(report)
	return component.MergeCandidates(collected, limit), nil
}

func (index *Index) readSnapshot(ctx context.Context, address projectionstore.Address) (snapshot, error) {
	if index == nil || index.store == nil {
		return snapshot{}, errors.New("vector projection: index is required")
	}
	// The table shares its entries with the store's kept base: the norms below
	// are the lane's own reading of it, so they are computed onto this copy
	// rather than written into the state the store holds.
	build, _, err := index.store.MaterializeView(ctx, address, index.storageProjection)
	if err != nil {
		return snapshot{}, fmt.Errorf("vector projection: %w", err)
	}
	if err := fillNorms(&build); err != nil {
		return snapshot{}, err
	}
	return build, nil
}

// fillNorms computes every entry's L2 norm once per materialized table, so the
// scan scores with a stored norm instead of recomputing one per entry. The lane
// holds no table of its own, so this is paid per query: on a table of the size
// one shard holds -- a conversation's 800-odd entries at 2048 dimensions -- it
// is about 1.7M multiply-adds, one millisecond, against a scan that reads every
// one of those entries. A second copy of the norms, kept resident to save that
// millisecond, would be the memory this lane is built not to hold.
//
// A vector that has no norm is rejected here, with the entry named: the
// materialized state passed the lane's base validation, so this is the same
// condition the scan used to report, found once instead of once per query.
func fillNorms(build *snapshot) error {
	if len(build.Entries) == 0 {
		build.norms = nil
		return nil
	}
	norms := make([]float64, len(build.Entries))
	for index := range build.Entries {
		value, err := norm(build.Entries[index].Vector)
		if err != nil {
			return fmt.Errorf("vector projection: entry %q: %w", build.Entries[index].ID, err)
		}
		norms[index] = value
	}
	build.norms = norms
	return nil
}

// scanReport is what one scan pass did: the entries it considered, the entries
// it scored, and the wall time scoring and ordering took. The phases are timed
// because an exact scan is the lane's own cost -- the alternative an
// approximate index would remove -- while loading a table and embedding the
// query are not.
type scanReport struct {
	scanned   int
	scored    int
	scanNanos time.Duration
	// scoreNanos is the part of the scan the cosine itself took over the
	// entries the filter admitted: their dot with the query, their norm when
	// the table does not carry one (a table the lane read does), and placing
	// the score where the order keeps or drops it. The rest of the scan is the
	// walk -- the filter match per entry -- which is what an approximate index
	// would also have to look at, differently.
	scoreNanos time.Duration
	// sortNanos is what it cost to turn the scores into the returned result:
	// ordering what the scan kept, and materializing those candidates. A scan
	// with a limit keeps at most the limit, so this is the work of k entries,
	// not of every match.
	sortNanos time.Duration
}

func searchSnapshot(build snapshot, query []float32, filter component.VectorSearchFilter, limit int) ([]component.Candidate, scanReport, error) {
	var report scanReport
	if len(build.Entries) == 0 {
		return []component.Candidate{}, report, nil
	}
	if len(query) != build.Dimensions {
		return nil, report, fmt.Errorf("vector projection: query dimension %d does not match index dimension %d", len(query), build.Dimensions)
	}
	queryNorm, err := norm(query)
	if err != nil {
		return nil, report, fmt.Errorf("vector projection: query: %w", err)
	}
	return scoreSnapshot(build, query, queryNorm, filter, limit)
}

// scoreSnapshot computes the cosine of every entry the filter admits and
// reports what the pass cost, keeping the candidates the limit asks for. The
// table's stored norms are used when it carries them, and computed per entry
// when it does not.
func scoreSnapshot(
	build snapshot,
	query []float32,
	queryNorm float64,
	filter component.VectorSearchFilter,
	limit int,
) ([]component.Candidate, scanReport, error) {
	report := scanReport{scanned: len(build.Entries)}
	started := time.Now()
	selection := newSelection(limit, len(build.Entries))
	norms := build.norms
	if len(norms) != len(build.Entries) {
		norms = nil
	}
	for index, item := range build.Entries {
		if filter.Name != "" && item.Name != filter.Name {
			continue
		}
		matches, err := projectionstore.MatchesRequest(filter.Metadata, item.Address)
		if err != nil {
			return nil, report, fmt.Errorf("vector projection: selector: %w", err)
		}
		if !matches {
			continue
		}
		scoreStarted := time.Now()
		itemNorm, err := entryNorm(norms, index, item.Vector)
		if err != nil {
			return nil, report, fmt.Errorf("vector projection: entry %q: %w", item.ID, err)
		}
		var dot float64
		for i := range query {
			dot += float64(query[i]) * float64(item.Vector[i])
		}
		cosine := dot / (queryNorm * itemNorm)
		if math.IsNaN(cosine) || math.IsInf(cosine, 0) {
			return nil, report, fmt.Errorf("vector projection: entry %q produced non-finite cosine", item.ID)
		}
		cosine = max(-1, min(1, cosine))
		report.scored++
		selection.offer(ranked{index: index, id: item.ID, score: cosine})
		report.scoreNanos += time.Since(scoreStarted)
	}
	report.scanNanos = time.Since(started)
	orderStarted := time.Now()
	kept := selection.result()
	results := make([]component.Candidate, len(kept))
	for position, candidate := range kept {
		item := build.Entries[candidate.index]
		results[position] = component.Candidate{
			ID: item.ID, Lane: laneName, Name: item.Name, Score: candidate.score,
			Source: item.Source, Address: item.Address, Metadata: item.Metadata.Clone(),
		}
	}
	report.sortNanos = time.Since(orderStarted)
	return results, report, nil
}

// ranked is one entry the scan scored: where it sits in the table, the id that
// breaks a tie, and its cosine.
type ranked struct {
	index int
	id    string
	score float64
}

// ranksAfter reports whether left comes after right in the order the scan
// publishes: a lower score, and on an equal score a higher id. Entry ids are
// unique in a table, so this is a total order: two candidates never rank
// neither before nor after each other.
func (left ranked) ranksAfter(right ranked) bool {
	if left.score != right.score {
		return left.score < right.score
	}
	return left.id > right.id
}

// selection keeps the best candidates a scan has seen, without holding a
// candidate for every entry the filter admitted. It is a min-heap under
// "ranks after" -- the root is the entry the selection would drop first -- so
// an entry that beats the root replaces it in time logarithmic in the limit
// instead of the scan sorting every match at the end.
//
// A selection with no bound keeps everything and orders it at the end, which
// is what a scan that asks for the whole table does. The set a bounded
// selection holds is exactly the limit best entries: the root is always the
// worst one kept, so dropping and replacing it can never drop an entry that
// ranks before one it keeps.
type selection struct {
	limit int
	items []ranked
}

// newSelection sizes a selection for a limit and a table. A limit that cannot
// prune -- not set, or at least as large as the table -- keeps everything.
func newSelection(limit, entries int) selection {
	if limit <= 0 || limit >= entries {
		return selection{items: make([]ranked, 0, entries)}
	}
	return selection{limit: limit, items: make([]ranked, 0, limit)}
}

// offer adds one scored entry to the selection.
func (selection *selection) offer(candidate ranked) {
	if selection.limit <= 0 {
		selection.items = append(selection.items, candidate)
		return
	}
	if len(selection.items) < selection.limit {
		selection.items = append(selection.items, candidate)
		selection.siftUp(len(selection.items) - 1)
		return
	}
	if !selection.items[0].ranksAfter(candidate) {
		// The entry the selection would drop first ranks before this one:
		// there is nothing here to keep.
		return
	}
	selection.items[0] = candidate
	selection.siftDown(0)
}

// result returns what the selection kept, in the order the scan publishes.
func (selection *selection) result() []ranked {
	kept := selection.items
	sort.Slice(kept, func(i, j int) bool { return kept[j].ranksAfter(kept[i]) })
	return kept
}

// siftUp restores the heap after an entry was appended: the worst entries sit
// at the root, so an entry that ranks after its parent moves up.
func (selection *selection) siftUp(index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !selection.items[index].ranksAfter(selection.items[parent]) {
			return
		}
		selection.items[index], selection.items[parent] = selection.items[parent], selection.items[index]
		index = parent
	}
}

// siftDown restores the heap after the root was replaced: the worst child
// takes the vacated place, and the entry sinks until no child ranks after it.
func (selection *selection) siftDown(index int) {
	for {
		left := 2*index + 1
		if left >= len(selection.items) {
			return
		}
		worst := left
		if right := left + 1; right < len(selection.items) && selection.items[right].ranksAfter(selection.items[left]) {
			worst = right
		}
		if !selection.items[worst].ranksAfter(selection.items[index]) {
			return
		}
		selection.items[index], selection.items[worst] = selection.items[worst], selection.items[index]
		index = worst
	}
}

// entryNorm returns the entry's L2 norm: the one the table already holds when
// it was filled, and a fresh computation when it was not.
func entryNorm(stored []float64, index int, vector []float32) (float64, error) {
	if stored != nil {
		return stored[index], nil
	}
	return norm(vector)
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

// tableBytes weighs one table for the store's cache budget: its vector payload,
// four bytes per dimension per entry. That is the term that dominates a table
// and the one a host serving many conversations has to bound -- a measured
// 9324-entry, 2048-dimension table is 72.8MB of payload -- and it is the only
// term counted here: entry identity, addresses and metadata sit on top, so a
// budget sized in payload under-counts what the cache really holds.
func tableBytes(build snapshot) int64 {
	return int64(len(build.Entries)) * int64(build.Dimensions) * 4
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

// sanitizeEmbedContent keeps only parts the embedding model can consume
// (text and image) and drops eval-only data parts and tool parts.
func sanitizeEmbedContent(content coremessage.Content) coremessage.Content {
	var parts []coremessage.Part
	for _, part := range content.Parts {
		switch part.Kind() {
		case coremessage.PartText, coremessage.PartImage:
			parts = append(parts, part)
		}
	}
	return coremessage.Content{Parts: parts}
}

// embedWithTextFallback embeds a batch, and retries it text-only when the
// provider refuses it. A single image the multimodal endpoint rejects -- an
// HTML block page served as image/jpeg, an unsupported codec, an oversized
// payload -- used to abort the whole derivation pass for that conversation, and
// it failed deterministically on every retry. Images are a best-effort
// enrichment; text always embeds, so the batch degrades instead of stalling.
func (index *Index) embedWithTextFallback(ctx context.Context, items []inference.EmbedItem) (inference.EmbedResponse, error) {
	request := inference.EmbedRequest{Items: items}
	response, err := index.runtime.Embed(ctx, index.model, request)
	if err == nil {
		if validateErr := response.ValidateFor(request); validateErr == nil {
			return response, nil
		} else {
			err = validateErr
		}
	}
	fallback := make([]inference.EmbedItem, len(items))
	changed := false
	for i, item := range items {
		fallback[i] = inference.EmbedItem{Content: textOnlyEmbedContent(item.Content)}
		if len(fallback[i].Content.Parts) != len(item.Content.Parts) {
			changed = true
		}
	}
	if !changed {
		return inference.EmbedResponse{}, err
	}
	fallbackRequest := inference.EmbedRequest{Items: fallback}
	retry, retryErr := index.runtime.Embed(ctx, index.model, fallbackRequest)
	if retryErr != nil {
		return inference.EmbedResponse{}, err
	}
	if retryValidateErr := retry.ValidateFor(fallbackRequest); retryValidateErr != nil {
		return inference.EmbedResponse{}, err
	}
	return retry, nil
}

// textOnlyEmbedContent drops the image parts, substituting a placeholder for an
// item that was image-only so the fallback request stays valid.
func textOnlyEmbedContent(content coremessage.Content) coremessage.Content {
	parts := make([]coremessage.Part, 0, len(content.Parts))
	for _, part := range content.Parts {
		if part.Kind() == coremessage.PartText {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, coremessage.TextPart{Text: "[image omitted]"})
	}
	return coremessage.Content{Parts: parts}
}

// generationProjection isolates vectors produced by different embedding
// identities. A new model starts an empty generation and is filled by normal
// source replay; retrieval degrades through the other lanes until it exists.
// The payload form is part of the identity for the same reason: state an
// earlier form published is not state this build can read, and a new name is
// how the lane says so without touching what a build that still reads it holds.
func generationProjection(projection string, model model.ModelRef) string {
	payload := strings.Join([]string{
		AlgorithmVersion, EncodingVersion, model.ID.Provider, model.ID.Name, model.Profile,
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%s.vector-%x", projection, sum[:12])
}

// generationIdentity names the producer of one vector generation, readably:
// the scoring algorithm and the payload form, together with the embedding model
// identity the generation is bound to.
func generationIdentity(model model.ModelRef) string {
	parts := []string{AlgorithmVersion, EncodingVersion, model.ID.Provider, model.ID.Name}
	if model.Profile != "" {
		parts = append(parts, model.Profile)
	}
	return strings.Join(parts, "+")
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

func requestContent(query string) coremessage.Content {
	return coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: query}}}
}
