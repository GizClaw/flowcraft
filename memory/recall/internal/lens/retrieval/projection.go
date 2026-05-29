// Package retrieval implements the canonical retrieval projection.
//
// It is a write-time derived view of the temporal ledger: facts are
// flattened into retrieval.Doc shape and pushed into a retrieval.Index
// backend. Recall reads from the index through a CandidateSource, never
// the other way around — retrieval here is a search backend, not a
// truth layer (docs §8.1).
package retrieval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/errdefs"

	"github.com/GizClaw/flowcraft/memory/retrieval"
)

const (
	DocKindMetadataKey    = "retrieval_doc_kind"
	DocKindFact           = "fact"
	DocKindEvidence       = "evidence"
	EvidenceIDMetadataKey = "evidence_id"
)

// Projection is the canonical retrieval projection. It is Required:
// Save must not ack a write that is not searchable, otherwise the
// read path will silently miss freshly written facts.
type Projection struct {
	index    retrieval.Index
	embedder embedding.Embedder
}

// Option configures the projection at construction time. Options are
// purely additive — the projection works without any when the caller
// only wants BM25 indexing.
type Option func(*Projection)

// WithEmbedder enables semantic indexing. The projection will embed
// every fact's searchable content and store the vector in Doc.Vector
// so the index can run hybrid BM25 + cosine scoring. Embed failures
// degrade gracefully: the fact is still indexed (BM25 only) and a
// scope-level warning is emitted; no Save fails because of an
// embedder outage.
func WithEmbedder(e embedding.Embedder) Option {
	return func(p *Projection) {
		p.embedder = e
	}
}

// New constructs a retrieval Projection backed by index. Index ownership
// stays with the caller (Memory.Close closes the index, not the
// projection).
func New(index retrieval.Index, opts ...Option) (*Projection, error) {
	if index == nil {
		return nil, errdefs.Validationf("recall retrieval projection: index is required")
	}
	p := &Projection{index: index}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Name implements port.Projection.
func (p *Projection) Name() string { return "retrieval" }

// AcceptsKind rejects KindEpisode. Episode facts are raw conversation
// captures; routing them through retrieval would trigger external
// embedder calls on the sync write path (when WithEmbedder is set)
// and pollute search results with verbatim turn text.
func (p *Projection) AcceptsKind(k domain.FactKind) bool { return k != domain.KindEpisode }

// shouldIndexInRetrieval reports whether a fact belongs in the active
// retrieval index (historical, not episode, not superseded).
func shouldIndexInRetrieval(f domain.TemporalFact, now time.Time) bool {
	if f.ID == "" || f.Kind == domain.KindEpisode || f.CorrectedBy != "" {
		return false
	}
	return domain.IsHistorical(f, now)
}

// Consistency reports Required: a retrieval projection failure must
// fail the canonical write so callers do not see an empty Recall on
// a fact they just stored.
func (p *Projection) Consistency() port.Consistency { return port.Required }

// Project upserts canonical facts into the retrieval namespace. Facts
// in mixed scopes are grouped per namespace so each Upsert is scope
// local.
//
// The retrieval projection deliberately treats a superseded fact
// (CorrectedBy != "") as "not part of the active view" and silently
// skips it. Under normal Save flow this filter is a no-op (the
// resolver/store close the prior fact's validity *after* the new
// successor is projected), but rebuild and any future bulk-replay
// path may feed superseded facts in and must not put them back into
// the search index.
func (p *Projection) Project(ctx context.Context, facts []domain.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	now := time.Now()
	grouped := groupByNamespace(facts)
	for ns, group := range grouped {
		var superseded []string
		docs := make([]retrieval.Doc, 0, len(group))
		var evict []string
		var refreshFactIDs []any
		for _, f := range group {
			superseded = append(superseded, f.Supersedes...)
			if f.ID != "" && !shouldIndexInRetrieval(f, now) {
				evict = append(evict, f.ID)
				evict = append(evict, evidenceDocIDs(f)...)
			}
			if !shouldIndexInRetrieval(f, now) {
				continue
			}
			if f.ID != "" {
				refreshFactIDs = append(refreshFactIDs, f.ID)
			}
			docs = append(docs, toDocs(f)...)
		}
		if len(refreshFactIDs) > 0 {
			staleEvidence, err := listEvidenceDocIDs(ctx, p.index, ns, refreshFactIDs)
			if err != nil {
				return fmt.Errorf("retrieval projection list evidence docs ns=%s: %w", ns, err)
			}
			evict = append(evict, staleEvidence...)
		}
		toDelete := uniqueStrings(append(superseded, evict...))
		if len(toDelete) > 0 {
			if err := p.index.Delete(ctx, ns, toDelete); err != nil {
				return fmt.Errorf("retrieval projection delete ns=%s: %w", ns, err)
			}
		}
		if len(docs) == 0 {
			continue
		}
		// Vector embedding runs via SideEffectEmbeddingBackfill outside
		// the scope write lock; lexical docs land here only.
		if err := p.index.Upsert(ctx, ns, docs); err != nil {
			return fmt.Errorf("retrieval projection upsert ns=%s: %w", ns, err)
		}
	}
	return nil
}

func listEvidenceDocIDs(ctx context.Context, idx retrieval.Index, namespace string, factIDs []any) ([]string, error) {
	const pageSize = 256
	filter := retrieval.Filter{
		Eq: map[string]any{
			DocKindMetadataKey: DocKindEvidence,
		},
		In: map[string][]any{
			domain.MetaFactID: factIDs,
		},
	}
	var (
		out  []string
		page string
	)
	for {
		resp, err := idx.List(ctx, namespace, retrieval.ListRequest{
			Filter:    filter,
			PageSize:  pageSize,
			PageToken: page,
			OrderBy:   retrieval.OrderByIDAsc,
		})
		if err != nil {
			return nil, err
		}
		if resp == nil {
			break
		}
		for _, doc := range resp.Items {
			if doc.ID != "" {
				out = append(out, doc.ID)
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		page = resp.NextPageToken
	}
	return out, nil
}

// attachEmbeddings populates docs[i].Vector with the embedding of the
// fact's natural-language Content (the LLM-extracted one-sentence
// summary), NOT the full BM25 indexing text. This is deliberate:
//
//   - the vector lane carries "semantic similarity" signal, which is
//     strongest on clean prose. Concatenating Content + S/P/O +
//     entities + evidence quote (the BM25 text) dilutes that signal
//     with keyword noise and pushes most facts toward truncation
//     limits of common embedders.
//   - entity / participant / location lookup is already handled by
//     the structured candidate sources (entity / relation / profile /
//     graph). The vector lane does not need to duplicate them.
//
// Facts whose canonical Content is empty fall back to a short
// triple-derived sentence (subject predicate object) so they still
// participate in the vector lane; if even that is empty the fact is
// skipped (BM25-only for that doc).
//
// Embedder failure modes are handled defensively:
//   - EmbedBatch returns an error → retry per-text via Embed so a
//     single bad input doesn't strand the whole batch (mirrors v1's
//     embedBatch helper);
//   - EmbedBatch returns fewer vectors than inputs → treat the tail
//     as missing (skip) rather than panic on out-of-range access.
//
// Save never fails because the embedder is offline or rate-limited;
// the affected facts simply index for BM25 only.
func (p *Projection) attachEmbeddings(ctx context.Context, facts []domain.TemporalFact, docs []retrieval.Doc) {
	// TODO: when Projection gains hook access, emit
	// Status=Degraded here instead of silently falling back to
	// BM25-only. See internal-docs/recall-v2-architecture-debts.md §3.
	if len(docs) == 0 || len(facts) != len(docs) {
		return
	}
	texts := make([]string, 0, len(docs))
	idxs := make([]int, 0, len(docs))
	for i := range docs {
		text := embedTextFor(facts[i])
		if text == "" {
			continue
		}
		texts = append(texts, text)
		idxs = append(idxs, i)
	}
	if len(texts) == 0 {
		return
	}
	vecs := embedBatchWithFallback(ctx, p.embedder, texts)
	for i, v := range vecs {
		if len(v) == 0 || i >= len(idxs) {
			continue
		}
		docs[idxs[i]].Vector = v
	}
}

// embedTextFor picks the natural-language text the vector lane should
// embed for a fact. The hierarchy mirrors what an answer LLM would
// quote: canonical Content first, S/P/O sentence second, evidence
// quote last. Facts with none of these are skipped.
func embedTextFor(f domain.TemporalFact) string {
	if c := strings.TrimSpace(f.Content); c != "" {
		return c
	}
	parts := []string{}
	for _, s := range []string{f.Subject, f.Predicate, f.Object} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	return strings.TrimSpace(f.EvidenceText)
}

// embedBatchWithFallback calls EmbedBatch and falls back to per-text
// Embed when the batch fails or returns a partial result. This
// mirrors recall_v1's embedBatch and protects against providers that
// occasionally truncate batch responses or fail an entire batch when
// a single input is problematic.
//
// The returned slice always has len == len(texts); missing entries
// are zero-length so callers can detect skips without an extra map.
func embedBatchWithFallback(ctx context.Context, emb embedding.Embedder, texts []string) [][]float32 {
	if emb == nil || len(texts) == 0 {
		return nil
	}
	out := make([][]float32, len(texts))
	vecs, err := emb.EmbedBatch(ctx, texts)
	if err == nil && len(vecs) == len(texts) {
		return vecs
	}
	// Partial / failed batch: try one-by-one. We still return what we
	// can — a single per-text failure does not abort the rest, which
	// matches the projection's "best-effort vector lane" contract.
	for i, t := range texts {
		if i < len(vecs) && len(vecs[i]) > 0 {
			out[i] = vecs[i]
			continue
		}
		v, e := emb.Embed(ctx, t)
		if e != nil {
			continue
		}
		out[i] = v
	}
	return out
}

// Forget removes facts by id within a scope-derived namespace.
func (p *Projection) Forget(ctx context.Context, scope domain.Scope, factIDs []string) error {
	if len(factIDs) == 0 {
		return nil
	}
	ns := NamespaceFor(scope)
	if err := p.index.Delete(ctx, ns, factIDs); err != nil {
		return fmt.Errorf("retrieval projection delete ns=%s: %w", ns, err)
	}
	return nil
}

// ClearScope deletes every doc in the scope namespace by paginating
// the index list and issuing batched Deletes. Backs Memory.ForgetAll
// and is idempotent on an already-empty namespace.
func (p *Projection) ClearScope(ctx context.Context, scope domain.Scope) error {
	ns := NamespaceFor(scope)
	ids, err := listAllDocIDs(ctx, p.index, ns)
	if err != nil {
		return fmt.Errorf("retrieval projection clear ns=%s list: %w", ns, err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := p.index.Delete(ctx, ns, ids); err != nil {
		return fmt.Errorf("retrieval projection clear ns=%s delete: %w", ns, err)
	}
	return nil
}

// Rebuild applies exact-replace semantics within the supplied scope:
// docs present in the index but missing from the *active* slice of
// the snapshot are deleted, then the active subset is upserted. A
// fact is "active" iff CorrectedBy == "" — the same rule Project
// enforces. The caller passes the full IncludeSuperseded=true
// snapshot (Memory layer does not pre-filter), and the projection
// decides what belongs in its active view.
//
// Note: facts in the snapshot must all share scope. Multi-scope
// snapshots should be split by the caller (Memory.Rebuild handles
// this internally).
func (p *Projection) Rebuild(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
	ns := NamespaceFor(scope)
	now := time.Now()
	active := make([]domain.TemporalFact, 0, len(facts))
	activeIDs := make(map[string]struct{}, len(facts))
	for _, f := range facts {
		if !shouldIndexInRetrieval(f, now) {
			continue
		}
		active = append(active, f)
		activeIDs[f.ID] = struct{}{}
		for _, id := range evidenceDocIDs(f) {
			activeIDs[id] = struct{}{}
		}
	}

	existing, err := listAllDocIDs(ctx, p.index, ns)
	if err != nil {
		return fmt.Errorf("retrieval projection rebuild ns=%s list: %w", ns, err)
	}
	var stale []string
	for _, id := range existing {
		if _, keep := activeIDs[id]; !keep {
			stale = append(stale, id)
		}
	}
	if len(stale) > 0 {
		if err := p.index.Delete(ctx, ns, stale); err != nil {
			return fmt.Errorf("retrieval projection rebuild ns=%s delete stale: %w", ns, err)
		}
	}
	if len(active) == 0 {
		return nil
	}
	return p.Project(ctx, active)
}

// BackfillEmbeddings populates Doc.Vector for facts already indexed
// lexically. It is invoked from the side-effect outbox drain path.
func (p *Projection) BackfillEmbeddings(ctx context.Context, facts []domain.TemporalFact) error {
	if p == nil || p.embedder == nil || len(facts) == 0 {
		return nil
	}
	now := time.Now()
	grouped := groupByNamespace(facts)
	for ns, group := range grouped {
		active := make([]domain.TemporalFact, 0, len(group))
		docs := make([]retrieval.Doc, 0, len(group))
		for _, f := range group {
			if !shouldIndexInRetrieval(f, now) {
				continue
			}
			active = append(active, f)
			docs = append(docs, toDoc(f))
		}
		if len(docs) == 0 {
			continue
		}
		p.attachEmbeddings(ctx, active, docs)
		if err := p.index.Upsert(ctx, ns, docs); err != nil {
			return fmt.Errorf("retrieval embedding backfill ns=%s: %w", ns, err)
		}
	}
	return nil
}

// listAllDocIDs paginates retrieval.Index.List to enumerate every
// doc id currently stored in namespace. We page in modest batches so
// large namespaces do not require backend-specific scan support.
func listAllDocIDs(ctx context.Context, idx retrieval.Index, namespace string) ([]string, error) {
	const pageSize = 256
	var (
		out  []string
		page string
	)
	for {
		resp, err := idx.List(ctx, namespace, retrieval.ListRequest{
			PageSize:  pageSize,
			PageToken: page,
			OrderBy:   retrieval.OrderByIDAsc,
		})
		if err != nil {
			return nil, err
		}
		if resp == nil {
			break
		}
		for _, d := range resp.Items {
			out = append(out, d.ID)
		}
		if resp.NextPageToken == "" {
			break
		}
		page = resp.NextPageToken
	}
	return out, nil
}

func groupByNamespace(facts []domain.TemporalFact) map[string][]domain.TemporalFact {
	out := make(map[string][]domain.TemporalFact)
	for _, f := range facts {
		ns := NamespaceFor(f.Scope)
		out[ns] = append(out[ns], f)
	}
	return out
}

func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// toDoc flattens a fact into a retrieval Doc. Reserved metadata keys
// are owned by the projection: user-supplied Metadata is copied first
// so reserved keys always win when there is overlap.
func toDoc(f domain.TemporalFact) retrieval.Doc {
	meta := make(map[string]any, len(f.Metadata)+12)
	for k, v := range f.Metadata {
		meta[k] = v
	}

	meta[domain.MetaFactID] = f.ID
	meta[DocKindMetadataKey] = DocKindFact
	meta[domain.MetaFactKind] = string(f.Kind)
	meta[domain.MetaScopeRT] = f.Scope.RuntimeID
	meta[domain.MetaScopeUser] = f.Scope.UserID
	meta[domain.MetaScopeAgent] = f.Scope.AgentID
	meta[domain.MetaMergeKey] = f.MergeKey
	meta[domain.MetaConfidence] = f.Confidence
	if f.Reinforcement > 0 {
		meta[domain.MetaReinforcement] = f.Reinforcement
	}
	if f.Penalty > 0 {
		meta[domain.MetaPenalty] = f.Penalty
	}
	meta[domain.MetaObservedAt] = f.ObservedAt.UnixMilli()
	if f.ValidFrom != nil {
		meta[domain.MetaValidFrom] = f.ValidFrom.UnixMilli()
	}
	if f.ValidTo != nil {
		meta[domain.MetaValidTo] = f.ValidTo.UnixMilli()
	}
	if f.CorrectedBy != "" {
		meta[domain.MetaCorrectedBy] = f.CorrectedBy
	}
	if len(f.Entities) > 0 {
		entities := make([]any, len(f.Entities))
		for i, e := range f.Entities {
			entities[i] = e
		}
		meta[domain.MetaEntities] = entities
	}

	return retrieval.Doc{
		ID:        f.ID,
		Content:   buildContent(f),
		Metadata:  meta,
		Timestamp: pickTimestamp(f),
	}
}

func toDocs(f domain.TemporalFact) []retrieval.Doc {
	factDoc := toDoc(f)
	docs := []retrieval.Doc{factDoc}
	for _, ref := range f.EvidenceRefs {
		id := evidenceID(ref)
		text := strings.TrimSpace(ref.Text)
		if id == "" || text == "" {
			continue
		}
		meta := make(map[string]any, len(factDoc.Metadata)+2)
		for k, v := range factDoc.Metadata {
			meta[k] = v
		}
		meta[DocKindMetadataKey] = DocKindEvidence
		meta[EvidenceIDMetadataKey] = id
		ts := ref.Timestamp
		if ts.IsZero() {
			ts = factDoc.Timestamp
		}
		docs = append(docs, retrieval.Doc{
			ID:        evidenceDocID(f.ID, id),
			Content:   evidenceDocContent(f, ref),
			Metadata:  meta,
			Timestamp: ts,
		})
	}
	return docs
}

func evidenceDocContent(f domain.TemporalFact, ref domain.EvidenceRef) string {
	parts := []string{ref.Text, f.Content, f.Subject, f.Predicate, f.Object, f.Location}
	parts = append(parts, f.Entities...)
	parts = append(parts, f.Participants...)
	return strings.Join(nonEmptyStrings(parts...), " ")
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func evidenceDocIDs(f domain.TemporalFact) []string {
	out := make([]string, 0, len(f.EvidenceRefs))
	for _, ref := range f.EvidenceRefs {
		if id := evidenceID(ref); id != "" && f.ID != "" {
			out = append(out, evidenceDocID(f.ID, id))
		}
	}
	return out
}

func evidenceID(ref domain.EvidenceRef) string {
	if strings.TrimSpace(ref.ID) != "" {
		return strings.TrimSpace(ref.ID)
	}
	return strings.TrimSpace(ref.MessageID)
}

func evidenceDocID(factID, evidenceID string) string {
	return factID + "#" + evidenceID
}

// buildContent renders the searchable text for a fact. The canonical
// fact content remains primary, but evidence grounding is also indexed
// so compressed LLM-extracted facts can still be found by source-level
// details such as exact dates, places, and phrasing.
func buildContent(f domain.TemporalFact) string {
	parts := make([]string, 0, 6+len(f.Entities)+len(f.Participants)+len(f.EvidenceRefs))
	appendPart := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" {
			parts = append(parts, s)
		}
	}

	appendPart(f.Content)
	appendPart(f.Subject)
	appendPart(f.Predicate)
	appendPart(f.Object)
	appendPart(domain.SemanticTextBlob(f))
	for _, e := range f.Entities {
		appendPart(e)
	}
	for _, p := range f.Participants {
		appendPart(p)
	}
	appendPart(f.Location)
	appendPart(f.EvidenceText)
	for _, ref := range f.EvidenceRefs {
		appendPart(ref.Text)
	}
	return strings.Join(parts, " ")
}

// pickTimestamp resolves Doc.Timestamp from valid_from -> observed_at.
func pickTimestamp(f domain.TemporalFact) time.Time {
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
		return *f.ValidFrom
	}
	return f.ObservedAt
}
