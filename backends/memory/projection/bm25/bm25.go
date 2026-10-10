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
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	corebm25 "github.com/GizClaw/flowcraft/core/utils/bm25"
)

const (
	laneName = "bm25"
	// AlgorithmVersion names the lane's algorithm identity: the Okapi scoring
	// formula together with the tokenizer its persisted terms come from. v2
	// tokenizes with the shared kernel, so terms an older tokenizer wrote are
	// only matchable once the lane is rebuilt.
	AlgorithmVersion = "okapi-bm25-v2"
)

type Thresholds = projectionstore.Thresholds

type Config struct {
	KV         storage.Store
	Projection string
	K1         float64
	B          float64
	Thresholds projectionstore.Thresholds
}

type Index struct {
	store      *projectionstore.Store[snapshot, projectionstore.EntryDelta[doc]]
	projection string
	k1         float64
	b          float64

	mu     sync.Mutex
	cached *cachedKernel
}

// cachedKernel is the last kernel index the lane built, with the
// (identity, selector) pair it was built for. Identity is the store's
// content key — the same one its materialized cache trusts — and the
// selector fingerprint covers the metadata keys MatchesRequest narrows
// on.
type cachedKernel struct {
	identity string
	selector string
	kernel   *corebm25.Index
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
			Thresholds: config.Thresholds,
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
	return &Index{store: store, projection: config.Projection, k1: k1, b: b}, nil
}

func (index *Index) Rebuild(ctx context.Context, request component.ProjectionRequest) error {
	return index.FullRebuild(ctx, request)
}

func (index *Index) FullRebuild(ctx context.Context, request component.ProjectionRequest) error {
	if err := index.validateRequest(request); err != nil {
		return err
	}
	documents := make([]doc, len(request.Artifacts))
	totalLength := 0
	for i, artifact := range request.Artifacts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("bm25 projection: artifact %d: %w", i, err)
		}
		tokens := corebm25.Tokenize(artifact.Content.Text())
		terms := make(map[string]int)
		for _, token := range tokens {
			terms[token]++
		}
		address := component.AddressFromArtifact(artifact)
		if err := address.Validate(); err != nil {
			return fmt.Errorf("bm25 projection: artifact %q address: %w", artifact.ID, err)
		}
		totalLength += len(tokens)
		documents[i] = doc{
			ID: artifact.ID, Name: string(artifact.Kind), Length: len(tokens), Terms: terms,
			Source: artifact.Sources[0], Address: address, Metadata: artifact.Metadata.Clone(),
			ContentDigest: artifactDigest(artifact),
		}
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].ID < documents[j].ID })
	average := 0.0
	if len(documents) > 0 {
		average = float64(totalLength) / float64(len(documents))
	}
	return index.store.FullRebuild(ctx, request.Scope, index.projection, snapshot{
		K1: index.k1, B: index.b, AverageLength: average, Documents: documents,
	}, documentsDigest(documents))
}

func (index *Index) ApplyDelta(ctx context.Context, delta component.ProjectionDelta) error {
	if err := delta.ValidateDocumentAddresses(); err != nil {
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
		tokens := corebm25.Tokenize(artifact.Content.Text())
		terms := make(map[string]int)
		for _, token := range tokens {
			terms[token]++
		}
		address := component.AddressFromArtifact(artifact)
		if err := address.Validate(); err != nil {
			return fmt.Errorf("bm25 projection: artifact %q address: %w", artifact.ID, err)
		}
		upserts = append(upserts, doc{
			ID: artifact.ID, Name: string(artifact.Kind), Length: len(tokens), Terms: terms,
			Source: artifact.Sources[0], Address: address,
			Metadata: artifact.Metadata.Clone(), ContentDigest: artifactDigest(artifact),
		})
	}
	_, err := index.store.ApplyDelta(ctx, delta.Scope, index.projection,
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
	build, active, err := index.store.Materialize(ctx, request.Scope, index.projection)
	if err != nil {
		return nil, fmt.Errorf("bm25 projection: %w", err)
	}
	selected := make([]doc, 0, len(build.Documents))
	for _, document := range build.Documents {
		matches, err := projectionstore.MatchesRequest(request.Metadata, document.Address)
		if err != nil {
			return nil, fmt.Errorf("bm25 projection: selector: %w", err)
		}
		if matches {
			selected = append(selected, document)
		}
	}
	// Scoring belongs to the shared kernel (core/utils/bm25), which owns
	// one BM25 implementation for every catalog in the platform. It is
	// derived from exactly two inputs the lane can name — the build
	// identity and the selector — so the lane caches it under that pair.
	kernel, err := index.kernelFor(active.Identity, build, selected, request.Metadata)
	if err != nil {
		return nil, err
	}
	documents := make(map[string]doc, len(selected))
	for _, document := range selected {
		documents[document.ID] = document
	}
	ranked := kernel.Search(request.Query, request.Limit)
	results := make([]component.Candidate, 0, len(ranked))
	for _, hit := range ranked {
		document := documents[hit.ID]
		results = append(results, component.Candidate{
			ID: document.ID, Lane: laneName, Name: document.Name, Score: hit.Score,
			Source: document.Source, Address: document.Address, Metadata: document.Metadata.Clone(),
		})
	}
	return results, nil
}

// kernelFor returns the kernel index that scores one materialized build
// under one selector, reusing the previous kernel when neither changed.
//
// Both belong in the key. The build identity is the store's content key —
// the same one its materialized cache trusts — and the selector narrows
// the scored document set, while a kernel's average length and document
// frequencies are properties of that set: the same snapshot under a
// different selector is a different index.
//
// The entry holds the kernel only. A kernel keeps term statistics rather
// than document text, so a hit shares a small immutable structure instead
// of retaining the projection's documents, and the single entry bounds
// the cache the way the store's materialized cache is bounded.
//
// The lane supplies what only it knows: the per-document term frequencies
// it persisted, the snapshot's k1/b, the selector-narrowed document set
// and the absence of prefix matching. The splitter is the kernel's on both
// sides — the stored terms were built with Tokenize and Search tokenizes
// the query the same way — so the two sides cannot drift apart, at the
// price of a lane whose entries were persisted by an older tokenizer
// staying unmatchable until they are rebuilt.
func (index *Index) kernelFor(
	identity string,
	build snapshot,
	selected []doc,
	metadata corememory.Metadata,
) (*corebm25.Index, error) {
	selector := projectionstore.SelectorKey(metadata)
	index.mu.Lock()
	cached := index.cached
	index.mu.Unlock()
	if cached != nil && cached.identity == identity && cached.selector == selector {
		return cached.kernel, nil
	}
	indexed := make([]corebm25.TermDoc, 0, len(selected))
	for _, document := range selected {
		indexed = append(indexed, corebm25.TermDoc{ID: document.ID, Text: document.Terms})
	}
	// Input order is the kernel's tie-break, and the lane has always
	// broken ties by ID: sort first, then the kernel's stable ranking
	// reproduces that order.
	sort.Slice(indexed, func(i, j int) bool { return indexed[i].ID < indexed[j].ID })
	kernel, err := corebm25.NewFromTerms(indexed,
		corebm25.WithK1(build.K1), corebm25.WithB(build.B), corebm25.WithPrefixWeight(0))
	if err != nil {
		return nil, fmt.Errorf("bm25 projection: %w", err)
	}
	index.mu.Lock()
	index.cached = &cachedKernel{identity: identity, selector: selector, kernel: kernel}
	index.mu.Unlock()
	return kernel, nil
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
