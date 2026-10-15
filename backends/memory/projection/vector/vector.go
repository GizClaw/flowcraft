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
	"sync"
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
	laneName                = "vector"
	AlgorithmVersion        = "vector-cosine-v2"
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
	// snapshotCache keeps the materialized vector table per scope. Loading it
	// per query meant reading every entry through the workspace store's single
	// mutex, which dominated retrieval latency under concurrency (measured:
	// vector lane 14.7s/query at 4 workers vs 449ms for bm25). Writes clear the
	// cache, so a query can only see an up-to-date table.
	snapshotMu    sync.RWMutex
	snapshotCache map[string]snapshot
	// stats accumulates the phase counters Stats() publishes. It is written on
	// the serving path with atomics only, so no reader can perturb what the
	// lane serves.
	stats *statsState
}

// cachedSnapshot returns the scope's vector table, loading it once per write
// epoch instead of once per query. loaded reports whether the table was
// materialized off the store rather than served from the cache.
func (index *Index) cachedSnapshot(ctx context.Context, scope corememory.Scope) (snapshot, bool, error) {
	key := scope.HardPartitionKey()
	index.snapshotMu.RLock()
	cached, ok := index.snapshotCache[key]
	index.snapshotMu.RUnlock()
	if ok {
		return cached, false, nil
	}
	build, err := index.readSnapshot(ctx, scope)
	if err != nil {
		return snapshot{}, false, err
	}
	index.snapshotMu.Lock()
	if index.snapshotCache == nil {
		index.snapshotCache = make(map[string]snapshot)
	}
	index.snapshotCache[key] = build
	index.snapshotMu.Unlock()
	return build, true, nil
}

// warmSnapshots loads the scope's table in the background so the first query
// does not pay for it. Deferred before invalidateScope, so the written scope's
// table is dropped first and then repopulated. The read is counted apart from the
// serving path -- a query's load has to stay the query's own -- but it pays
// the same materialization, so a reading that adds the two sees the whole
// scope's first read.
func (index *Index) warmSnapshots(scope corememory.Scope) {
	go func() {
		started := time.Now()
		_, loaded, err := index.cachedSnapshot(context.Background(), scope)
		// A warm that failed is recorded too: it is the reason a later query
		// pays a load of its own, and a warm nobody can see is how a state
		// that cannot be read stayed invisible.
		index.recordWarmLoad(time.Since(started), loaded, err != nil)
	}()
}

// invalidateScope drops one scope's cached table; called on a write to that
// scope, and by the tests that model one. A write changes exactly the
// partition it wrote -- state lives under the scope's own runtime, tenant, and
// agent -- so a table another scope holds is not what the write could stale.
//
// Dropping every scope's table on any write is what the lane did before, and
// it made a derivation of one conversation evict the tables of the others,
// which the next query of each paid to materialize (measured: 9.6s locally and
// 14.0s on the eval host for the 9324-entry table, per scope, per process).
func (index *Index) invalidateScope(scope corememory.Scope) {
	index.snapshotMu.Lock()
	delete(index.snapshotCache, scope.HardPartitionKey())
	index.snapshotMu.Unlock()
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
	Vector        []float32                  `json:"vector"`
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

func (index *Index) FullRebuild(ctx context.Context, request component.ProjectionRequest) error {
	defer index.warmSnapshots(request.Scope)
	defer index.invalidateScope(request.Scope)
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
		items[i] = inference.EmbedItem{Content: sanitizeEmbedContent(artifact.Content)}
	}
	response, err := index.embedWithTextFallback(ctx, items)
	if err != nil {
		return fmt.Errorf("vector projection: embed artifacts: %w", err)
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
	// Warm the cache for this scope after the write instead of leaving the
	// first query to pay the materialization (measured: the cold query cost up
	// to 22s while warm queries sat at ~0.6s).
	defer index.warmSnapshots(delta.Scope)
	defer index.invalidateScope(delta.Scope)
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
	index.recordQuery()
	loadStarted := time.Now()
	build, loaded, err := index.cachedSnapshot(ctx, request.Scope)
	index.recordSnapshotLoad(time.Since(loadStarted), loaded, err != nil)
	if err != nil {
		return nil, err
	}
	if len(build.Entries) == 0 {
		return []component.Candidate{}, nil
	}
	content := requestContent(request.Query)
	embedStarted := time.Now()
	response, err := index.runtime.Embed(ctx, index.model, inference.EmbedRequest{Items: []inference.EmbedItem{{Content: content}}})
	index.recordEmbed(time.Since(embedStarted))
	if err != nil {
		return nil, fmt.Errorf("vector projection: embed query: %w", err)
	}
	results, report, err := searchSnapshot(build, response.Embeddings[0].Vector, component.VectorSearchFilter{
		Metadata: request.Metadata,
	}, request.Limit)
	if err == nil {
		index.recordScan(report)
	}
	return results, err
}

// SearchVector searches persisted projection vectors with a precomputed query
// vector and never invokes the inference runtime.
func (index *Index) SearchVector(ctx context.Context, request component.VectorSearchRequest) ([]component.Candidate, error) {
	if err := validateVectorSearch(request); err != nil {
		return nil, err
	}
	index.recordQuery()
	loadStarted := time.Now()
	build, loaded, err := index.cachedSnapshot(ctx, request.Scope)
	index.recordSnapshotLoad(time.Since(loadStarted), loaded, err != nil)
	if err != nil {
		return nil, err
	}
	results, report, err := searchSnapshot(build, request.Vector, request.Filter, request.Limit)
	if err == nil {
		index.recordScan(report)
	}
	return results, err
}

func (index *Index) readSnapshot(ctx context.Context, scope corememory.Scope) (snapshot, error) {
	if index == nil || index.store == nil {
		return snapshot{}, errors.New("vector projection: index is required")
	}
	build, _, err := index.store.Materialize(ctx, scope, index.storageProjection)
	if err != nil {
		return snapshot{}, fmt.Errorf("vector projection: %w", err)
	}
	if err := fillNorms(&build); err != nil {
		return snapshot{}, err
	}
	return build, nil
}

// fillNorms computes every entry's L2 norm once per materialized table, so a
// query scores with a stored norm instead of recomputing one per entry per
// query. The norm is half of one cosine's floating-point work, and it is the
// half that depends on the entry alone: measured on a 9324-entry table, the
// cosine phase of a query was 7.2µs per scored entry, of which the norm was
// the vector-shaped part. The cost moves to the table's own load, where it is
// paid once per scope per write epoch instead of once per query.
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
func generationProjection(projection string, model model.ModelRef) string {
	payload := strings.Join([]string{
		AlgorithmVersion, model.ID.Provider, model.ID.Name, model.Profile,
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%s.vector-%x", projection, sum[:12])
}

// generationIdentity names the producer of one vector generation, readably:
// the scoring algorithm together with the embedding model identity the
// generation is bound to.
func generationIdentity(model model.ModelRef) string {
	parts := []string{AlgorithmVersion, model.ID.Provider, model.ID.Name}
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
