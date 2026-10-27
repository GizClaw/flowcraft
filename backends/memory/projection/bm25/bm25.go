// Package bm25 implements a Unicode-aware BM25 retrieval projection. Its
// persisted terms are the shared kernel's tokens (core/utils/bm25.Tokenize),
// the same splitter the kernel tokenizes queries with, so a stored document
// stays matchable without the lane owning a tokenizer of its own.
package bm25

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/errdefs"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	corebm25 "github.com/GizClaw/flowcraft/core/utils/bm25"
)

const (
	laneName = "bm25"
	// AlgorithmVersion names the lane's algorithm identity: the Okapi scoring
	// formula together with the tokenizer its persisted terms come from. v2
	// tokenizes with the shared kernel. An older tokenizer's ASCII terms
	// still match, because the two splitters agree token for token there; the
	// CJK terms it wrote as one long run stay out of reach of the characters
	// and bigrams a query produces now until the lane is rebuilt.
	AlgorithmVersion = "okapi-bm25-v2"
)

type Thresholds = projectionstore.Thresholds

type Config struct {
	KV         storage.Store
	Projection string
	K1         float64
	B          float64
	Thresholds projectionstore.Thresholds
	// CacheBytes bounds the tables the store behind this lane keeps decoded.
	// Zero uses the store's default budget.
	CacheBytes int64
}

type Index struct {
	store      *projectionstore.Store[snapshot, projectionstore.EntryDelta[doc]]
	projection string
	k1         float64
	b          float64
	// producerVersion is the identity of the state this index writes, composed
	// once at construction: the algorithm version above together with the
	// scoring parameters a persisted build carries.
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
	K1            float64 `json:"k1"`
	B             float64 `json:"b"`
	AverageLength float64 `json:"average_length"`
	Documents     []doc   `json:"documents"`
}

type doc struct {
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Length        int                        `json:"length"`
	Terms         map[string]int             `json:"terms"`
	Source        corememory.SourceRef       `json:"source"`
	Address       component.CandidateAddress `json:"address"`
	Metadata      corememory.Metadata        `json:"metadata,omitempty"`
	ContentDigest string                     `json:"content_digest"`
}

var _ component.Indexer = (*Index)(nil)
var _ component.DeltaIndexer = (*Index)(nil)
var _ component.FullRebuilder = (*Index)(nil)
var _ component.Searcher = (*Index)(nil)
var _ component.Producer = (*Index)(nil)
var _ component.Reclaimer = (*Index)(nil)

// producerVersion names the identity of the state one index writes: the
// algorithm version, which covers the tokenizer its persisted terms come from,
// plus the scoring parameters a build carries in its snapshot. k1 and b do not
// change which terms are persisted, but they do change what a persisted build
// scores with, and a delta apply rewrites them from live config without
// touching the manifest digests -- so recording them is what makes a parameter
// change visible instead of silent. A rebuild rewrites the snapshot with the
// parameters the running index was built with, which is why they belong to the
// identity a rebuild converges to rather than to a separate toggle.
func producerVersion(k1, b float64) string {
	return fmt.Sprintf("%s+k1=%s,b=%s", AlgorithmVersion,
		strconv.FormatFloat(k1, 'g', -1, 64), strconv.FormatFloat(b, 'g', -1, 64))
}

func New(config Config) (*Index, error) {
	if strings.TrimSpace(config.Projection) == "" {
		return nil, errors.New("bm25 projection: projection name is required")
	}
	k1 := config.K1
	b := config.B
	if k1 == 0 && b == 0 {
		k1 = 1.2
		b = 0.75
	} else if k1 == 0 {
		k1 = 1.2
	}
	if math.IsNaN(k1) || math.IsInf(k1, 0) || k1 <= 0 {
		return nil, errors.New("bm25 projection: k1 must be finite and positive")
	}
	if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 || b > 1 {
		return nil, errors.New("bm25 projection: b must be in [0,1]")
	}
	key := bm25EntryKey()
	store, err := projectionstore.NewTypedStore(config.KV, laneName,
		projectionstore.TypedOptions[snapshot, projectionstore.EntryDelta[doc]]{
			ProducerVersion: producerVersion(k1, b),
			Thresholds:      config.Thresholds,
			MaxBytes:        config.CacheBytes,
			BaseBytes:       tableBytes,
			Canonicalize: func(delta projectionstore.EntryDelta[doc]) projectionstore.EntryDelta[doc] {
				return projectionstore.CanonicalEntryDelta(delta, key)
			},
			ValidateBase: validateSnapshot,
			Apply: func(base *snapshot, delta projectionstore.EntryDelta[doc]) error {
				base.K1, base.B = k1, b
				base.Documents = projectionstore.ApplyEntryDelta(base.Documents, delta, key)
				recomputeAverage(base)
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("bm25 projection: %w", err)
	}
	return &Index{
		store: store, projection: config.Projection, k1: k1, b: b,
		producerVersion: producerVersion(k1, b),
	}, nil
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
// under for one scope, excluding the name this lane writes. The bm25 lane
// keeps one name, so the list is what a deployment that changed its projection
// name left behind.
func (index *Index) OtherStoredNames(ctx context.Context, scope corememory.Scope) ([]string, error) {
	return index.store.OtherNames(ctx, scope, index.projection)
}

// ReclaimSuperseded removes the bases and segments the lane compacted or
// rebuilt away, keeping the one the active manifest names. The bm25 lane names
// its storage after the projection setting alone, so the sweep is exactly the
// lane's own area: no generation of this family is something the lane is not.
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
		return fmt.Errorf("bm25 projection: %w", err)
	}
	covered := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		build, err := index.buildSnapshot(group.Artifacts)
		if err != nil {
			return err
		}
		if err := index.store.FullRebuild(ctx,
			projectionstore.Address{Scope: request.Scope, Shard: group.Shard}, index.projection,
			build, documentsDigest(build.Documents)); err != nil {
			return err
		}
		covered[group.Shard] = struct{}{}
	}
	// A rebuild replaces the lane for the scope, so a shard the artifact set no
	// longer covers is state the canonical log no longer has: it has to stop
	// being readable, or a deleted conversation keeps answering queries.
	if _, err := index.store.PruneShards(ctx, request.Scope, index.projection, covered); err != nil {
		return fmt.Errorf("bm25 projection: %w", err)
	}
	return nil
}

// buildSnapshot turns one shard's artifacts into its table: documents sorted by
// id, and the average length of the documents that shard holds.
//
// The average is the shard's own, and that is deliberate. Nothing scores against
// it: a read rebuilds the scoring set's statistics across every shard it plans
// (see Search). It stays in the snapshot because it is the one summary of a
// table a reader can compare against the documents it just decoded, which is what
// validateSnapshot does on every materialization.
func (index *Index) buildSnapshot(artifacts []component.Artifact) (snapshot, error) {
	documents := make([]doc, len(artifacts))
	totalLength := 0
	for i, artifact := range artifacts {
		if err := artifact.Validate(); err != nil {
			return snapshot{}, fmt.Errorf("bm25 projection: artifact %d: %w", i, err)
		}
		document, err := index.document(artifact)
		if err != nil {
			return snapshot{}, err
		}
		totalLength += document.Length
		documents[i] = document
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].ID < documents[j].ID })
	average := 0.0
	if len(documents) > 0 {
		average = float64(totalLength) / float64(len(documents))
	}
	return snapshot{K1: index.k1, B: index.b, AverageLength: average, Documents: documents}, nil
}

// document tokenizes one artifact into the lane's persisted form.
func (index *Index) document(artifact component.Artifact) (doc, error) {
	tokens := corebm25.Tokenize(artifact.Content.Text())
	terms := make(map[string]int)
	for _, token := range tokens {
		terms[token]++
	}
	address := component.AddressFromArtifact(artifact)
	if err := address.Validate(); err != nil {
		return doc{}, fmt.Errorf("bm25 projection: artifact %q address: %w", artifact.ID, err)
	}
	return doc{
		ID: artifact.ID, Name: string(artifact.Kind), Length: len(tokens), Terms: terms,
		Source: artifact.Sources[0], Address: address, Metadata: artifact.Metadata.Clone(),
		ContentDigest: artifactDigest(artifact),
	}, nil
}

func (index *Index) ApplyDelta(ctx context.Context, delta component.ProjectionDelta) error {
	if err := delta.ValidateDocumentAddresses(); err != nil {
		return fmt.Errorf("bm25 projection: %w", err)
	}
	if err := delta.ValidateShard(); err != nil {
		return fmt.Errorf("bm25 projection: %w", err)
	}
	if err := index.validateRequest(component.ProjectionRequest{Scope: delta.Scope, Projection: delta.Projection}); err != nil {
		return err
	}
	upserts := make([]doc, 0, len(delta.Upserts))
	for _, artifact := range delta.Upserts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("bm25 projection: artifact %q: %w", artifact.ID, err)
		}
		document, err := index.document(artifact)
		if err != nil {
			return err
		}
		upserts = append(upserts, document)
	}
	_, err := index.store.ApplyDelta(ctx,
		projectionstore.Address{Scope: delta.Scope, Shard: delta.Shard}, index.projection,
		projectionstore.EntryDelta[doc]{
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
	if strings.TrimSpace(request.Query) == "" {
		return []component.Candidate{}, nil
	}
	selector, err := projectionstore.ParseSelector(request.Metadata)
	if err != nil {
		return nil, fmt.Errorf("bm25 projection: selector: %w", err)
	}
	shards, err := index.searchShards(ctx, request.Scope, selector)
	if err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return []component.Candidate{}, nil
	}
	// A plan of one shard is materialized once and both passes read it: one
	// table fits the budget by construction, and scoring that table is the
	// read a single-table lane gave -- no write can land between the
	// statistics and the scoring. A plan of several shards cannot hold them
	// all (that is the memory the shards bound), so the passes re-read each
	// shard, and every shard is still read atomically on its own.
	var (
		single      snapshot
		singleFound bool
	)
	if len(shards) == 1 {
		single, singleFound, err = index.materialize(ctx, request.Scope, shards[0])
		if err != nil {
			return nil, err
		}
	}
	load := func(shard string) (snapshot, bool, error) {
		if len(shards) == 1 {
			return single, singleFound, nil
		}
		return index.materialize(ctx, request.Scope, shard)
	}
	// Pass 1 accumulates the statistics of the whole scoring set: how many
	// records the selector admits, how long they are together, and how many of
	// them contain each term. BM25's document frequencies and average length
	// are properties of the set being scored, not of the shard a reader happens
	// to hold, so they have to be counted across every planned shard before a
	// single record is scored. Scoring a shard against its own counts would
	// weigh the same term differently in the next shard, and the scores would
	// stop being comparable -- and merging incomparable scores is how a sharded
	// read silently reorders its results.
	statistics := corebm25.Aggregate{}
	k1, b := index.k1, index.b
	stamped := false
	for _, shard := range shards {
		build, found, err := load(shard)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if !stamped {
			// Every shard carries the parameters it was written with; they
			// agree because one lane wrote them all, and the first one names
			// them for the set.
			k1, b = build.K1, build.B
			stamped = true
		}
		shardStatistics := corebm25.Aggregate{
			TextTerms: make(map[string]int, len(build.Documents)),
		}
		for _, document := range build.Documents {
			if !selector.Matches(document.Address) {
				continue
			}
			shardStatistics.Documents++
			shardStatistics.TextLength += document.Length
			for term := range document.Terms {
				shardStatistics.TextTerms[term]++
			}
		}
		statistics.Add(shardStatistics)
	}
	// Scoring belongs to the shared kernel (core/utils/bm25), which owns one
	// BM25 implementation for every catalog in the platform. The lane supplies
	// what only it knows: the set's statistics, the parameters a build carries,
	// the per-document term frequencies it persisted, and the absence of prefix
	// matching. The splitter is the kernel's on both sides -- the stored terms
	// were built with Tokenize and the query is tokenized the same way -- so the
	// two sides cannot drift apart.
	scorer, err := corebm25.NewScorer(statistics,
		corebm25.WithK1(k1), corebm25.WithB(b), corebm25.WithPrefixWeight(0))
	if err != nil {
		return nil, fmt.Errorf("bm25 projection: %w", err)
	}
	query := corebm25.Tokenize(request.Query)
	// Pass 2 scores the same shards against the statistics pass 1 settled. Each
	// shard keeps its own best records and the merge keeps the top limit of the
	// union, so the result is the one a single table holding every planned shard
	// would produce: same scores, same order, same truncation.
	collected := make([][]component.Candidate, 0, len(shards))
	for _, shard := range shards {
		build, found, err := load(shard)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		results := make([]component.Candidate, 0, len(build.Documents))
		for _, document := range build.Documents {
			if !selector.Matches(document.Address) {
				continue
			}
			score := scorer.Score(document.Terms, document.Length, query)
			if score <= 0 {
				continue
			}
			results = append(results, component.Candidate{
				ID: document.ID, Lane: laneName, Name: document.Name, Score: score,
				Source: document.Source, Address: document.Address, Metadata: document.Metadata.Clone(),
			})
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
		return nil, fmt.Errorf("bm25 projection: selector: %w", err)
	}
	if !plan.All {
		return plan.Shards, nil
	}
	shards, err := index.store.Shards(ctx, scope, index.projection)
	if err != nil {
		return nil, fmt.Errorf("bm25 projection: %w", err)
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
		return snapshot{}, false, fmt.Errorf("bm25 projection: %w", err)
	}
	return build, true, nil
}

func (index *Index) validateRequest(request component.ProjectionRequest) error {
	if index == nil || index.store == nil {
		return errors.New("bm25 projection: index is required")
	}
	if request.Projection != "" && request.Projection != index.projection {
		return errors.New("bm25 projection: projection name mismatch")
	}
	return request.Scope.Validate()
}

func validateSearch(request component.SearchRequest) error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if request.Limit < 0 {
		return errors.New("bm25 projection: limit must not be negative")
	}
	return nil
}

func validateSnapshot(build snapshot) error {
	if build.K1 <= 0 || build.B < 0 || build.B > 1 || build.AverageLength < 0 {
		return errors.New("invalid BM25 parameters")
	}
	seen := make(map[string]struct{}, len(build.Documents))
	for _, document := range build.Documents {
		if document.ID == "" || document.ContentDigest == "" || document.Length < 0 {
			return errors.New("invalid document")
		}
		if _, exists := seen[document.ID]; exists {
			return fmt.Errorf("duplicate document %q", document.ID)
		}
		seen[document.ID] = struct{}{}
		if err := document.Source.Validate(); err != nil {
			return err
		}
		if err := document.Address.Validate(); err != nil {
			return err
		}
		total := 0
		for term, count := range document.Terms {
			if term == "" || count <= 0 {
				return errors.New("invalid term frequency")
			}
			total += count
		}
		if total != document.Length {
			return errors.New("document length does not match term frequencies")
		}
	}
	return nil
}

func recomputeAverage(build *snapshot) {
	totalLength := 0
	for _, document := range build.Documents {
		totalLength += document.Length
	}
	build.AverageLength = 0
	if len(build.Documents) > 0 {
		build.AverageLength = float64(totalLength) / float64(len(build.Documents))
	}
}

const (
	// postingBytes is what one stored posting costs beyond the term's own text:
	// the map entry, the count, and the header the string is held under.
	postingBytes = 24
	// documentBytes is what one document costs beyond its postings: its id, its
	// content digest, its source reference, and its address.
	documentBytes = 200
)

// tableBytes weighs one table for the store's cache budget. A BM25 base is a
// document per entry and a count per distinct term, so the weight is the
// postings' text plus a fixed cost each, and a fixed cost per document. The
// constants are estimates of the Go runtime's own overhead, not a measurement
// of it: a budget is a bound on what the lane holds, and an estimate that
// tracks the postings is what makes one table twice another's size count twice.
func tableBytes(build snapshot) int64 {
	weight := int64(0)
	for _, document := range build.Documents {
		weight += documentBytes
		for term := range document.Terms {
			weight += int64(len(term)) + postingBytes
		}
	}
	return weight
}

func bm25EntryKey() projectionstore.EntryKey[doc] {
	return projectionstore.EntryKey[doc]{
		ID: func(value doc) string { return value.ID },
		Document: func(value doc) component.DocumentAddress {
			return component.DocumentAddress{DatasetID: value.Address.DatasetID, DocumentID: value.Address.DocumentID}
		},
	}
}

func artifactDigest(artifact component.Artifact) string {
	sum := sha256.Sum256([]byte(artifact.Content.Text()))
	return fmt.Sprintf("%x", sum[:])
}

func documentsDigest(documents []doc) string {
	hash := sha256.New()
	for _, document := range documents {
		_, _ = hash.Write([]byte(document.ID + "\x00" + document.ContentDigest + "\x00"))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
