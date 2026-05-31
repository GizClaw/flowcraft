package recall

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// contentHash returns an MD5 hex digest scoped by user, used by the byte-level
// dedup path on Save. Same content from different users may legitimately exist
// in parallel namespaces, but within one user we never want exact duplicates.
func contentHash(scope Scope, content string) string {
	h := md5.New()
	h.Write([]byte(scope.UserID))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(content)))
	return hex.EncodeToString(h.Sum(nil))
}

// dedupHashes returns hash → existingDocID for content hashes already present
// in the namespace. Implemented via Index.List with an In filter; backends
// without native filter pushdown fall back to client-side scans.
//
// Filter composition (must mirror Recall's reachability gate so dedup and
// retrieval agree on which rows are alive):
//
//   - In: MetaContentHash ∈ hashes — the actual candidate match.
//   - [TombstoneFilter] — exclude logically deleted rows; without this a
//     hash that belongs ONLY to tombstoned docs falsely counts as
//     "already present" and Save short-circuits, leaving the namespace
//     with no live copy of the just-saved fact (#158).
//   - [ExpireFilter] keyed on m.cfg.now() — exclude rows whose
//     persisted expires_at metadata is in the past. Without this an expired row with
//     matching hash makes Save return success on the dedup_hit branch
//     while Recall's default ExpireFilter still hides it — i.e. Save
//     reported success but the fact is unreachable until the row gets
//     swept (#179.3). Same now() source the rest of the package (TTL
//     sweep, supersede, Recall) uses, so dedup's "alive" cutoff and the
//     readers' cutoff stay byte-identical.
func (m *lt) dedupHashes(ctx context.Context, scope Scope, hashes []string) (map[string]string, error) {
	out := make(map[string]string, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	ns := NamespaceFor(scope)
	in := make([]any, 0, len(hashes))
	for _, h := range hashes {
		in = append(in, h)
	}
	resp, err := m.idx.List(ctx, ns, retrieval.ListRequest{
		Filter: MergeFilters(
			retrieval.Filter{In: map[string][]any{MetaContentHash: in}},
			TombstoneFilter(),
			ExpireFilter(m.cfg.now()),
		),
		PageSize: len(hashes),
	})
	if err != nil || resp == nil {
		return out, err
	}
	for _, d := range resp.Items {
		if d.Metadata == nil {
			continue
		}
		if v, ok := d.Metadata[MetaContentHash].(string); ok {
			out[v] = d.ID
		}
	}
	return out, nil
}

// supersedeNeighbours dispatches to one of two deterministic supersede
// channels:
//
//   - slot channel (supersedeBySlot): runs when the extractor populated
//     ExtractedFact.Subject AND Predicate. Independent of the embedder
//     and the vector; controlled by [WithoutSlotChannel].
//   - vector+entity channel (the body below): runs only when slot fields
//     are absent. Requires an embedder and a non-empty vector; matches
//     candidates by entity-set equality AND cosine ≥ SoftMergeCosineMin.
//     Controlled by [WithoutSoftMerge].
//
// The two channels are intentionally orthogonal so callers can disable
// the noisier vector path while keeping the deterministic slot path
// (or vice versa). Old entries are NEVER deleted — history stays in
// the journal and Auditable.Rollback continues to work; retrieval-time
// damping is the responsibility of pipeline.SupersededDecay, which
// multiplies the score of any hit carrying MetaSupersededBy by its
// Factor (default 0.3).
//
// Episodic guard: the vector path SKIPS any fact whose categories mark
// it as an episodic / events record (see isEpisodicFact). The extractor
// prompt contract states "Episodic events MUST leave subject and
// predicate empty — they are append-only timeline data, not slot
// replacements", and the merger must honour the same contract. Without
// this guard, two events sharing the same actors / places but
// different dates (e.g. "Theo went to the photography club on
// 7 May 2023" vs "Theo went to the photography club on 8 May 2023")
// would collide on entity-set equality and cos ≥ SoftMergeCosineMin
// because the architecture-friendly extractor prompt deliberately
// keeps dates OUT of the entities field (they live in the fact body).
// The older event would then be damped at recall time and the
// downstream temporal / single-hop questions about it would fail.
// Stable-attribute facts that never reached the slot channel (e.g.
// "user enjoys X" with no controlled predicate) still benefit from the
// vector fallback.
//
// Execution surface: the vector path is a single-lane vector lookup
// that bypasses the configured pipeline by design (we only need
// cosines, not the ranked answer). It therefore never asks the
// backend for an Execution and never reads RawByRetriever; downstream
// callers should not treat it as part of the Recall explanation
// produced by [RecallExplainer.RecallExplain].
func (m *lt) supersedeNeighbours(
	ctx context.Context, scope Scope, newID string,
	fact ExtractedFact, vec []float32, now time.Time,
) {
	// Slot channel takes priority and is deterministic: it does not need
	// the embedder, the vector, or matching entities. When the extractor
	// declared a (subject, predicate) tuple AND neither side contains
	// the slot delimiter '|' the conflict signal is the tuple itself.
	// Tuples that contain '|' would produce ambiguous slot_keys
	// (see upsertFacts) and fall through to the vector / resolver
	// channels instead.
	if m.cfg.slotMerge && slotEligible(fact) {
		m.supersedeBySlot(ctx, scope, newID, fact, now)
		return
	}
	if !m.cfg.softMerge {
		return
	}
	// Episodic records are append-only — see godoc above. Even when the
	// older event has identical entities and high cosine similarity to
	// the newer one, the two must coexist (different timestamps, both
	// retrievable). Stable-attribute facts (preferences / opinions /
	// profile traits) remain eligible for the vector fallback.
	if isEpisodicFact(fact) {
		return
	}
	if m.cfg.embedder == nil || len(vec) == 0 {
		return
	}
	if len(fact.Entities) == 0 {
		return
	}
	ns := NamespaceFor(scope)
	resp, err := m.idx.Search(ctx, ns, retrieval.SearchRequest{
		QueryVector: vec,
		Filter:      AgentRecallFilter(scope),
		TopK:        m.cfg.softMergeTopK + 1, // +1 in case the new doc itself shows up
		// Debug intentionally left zero: see godoc above.
	})
	if err != nil || resp == nil {
		return
	}
	newEnts := lowerSet(fact.Entities)
	for _, h := range resp.Hits {
		if h.Doc.ID == newID {
			continue
		}
		// cos(a,b) is a metric-level signal exposed under "cos"; fall back to
		// h.Score when not present.
		cos := h.Score
		if h.Scores != nil {
			if v, ok := h.Scores["cos"]; ok {
				cos = v
			}
		}
		if cos < m.cfg.softMergeCosineMin {
			continue
		}
		oldEnts := lowerSet(docEntities(h.Doc))
		if !setEqual(newEnts, oldEnts) {
			continue
		}
		// Avoid re-superseding entries already pointing somewhere.
		if h.Doc.Metadata != nil {
			if v, ok := h.Doc.Metadata[MetaSupersededBy].(string); ok && v != "" {
				continue
			}
		}
		updated := h.Doc
		if updated.Metadata == nil {
			updated.Metadata = map[string]any{}
		}
		updated.Metadata[MetaSupersededBy] = newID
		updated.Metadata[MetaSupersededAt] = now.UnixMilli()
		if err := m.idx.Upsert(ctx, ns, []retrieval.Doc{updated}); err != nil {
			m.log("ltm: vector supersede upsert failed for %q: %v", h.Doc.ID, err)
			continue
		}
		supersedeTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("channel", "vector")))
	}
}

func docEntities(d retrieval.Doc) []string {
	if d.Metadata == nil {
		return nil
	}
	v, ok := d.Metadata[MetaEntities]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func lowerSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(strings.ToLower(s))
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

// supersedeBySlot is the deterministic supersede channel used when the
// extractor populates ExtractedFact.Subject and Predicate AND neither
// contains the slot delimiter '|'. It tags every older entry sharing
// the same slot_key with MetaSupersededBy=newID so that
// pipeline.SupersededDecay damps them at recall time. Old entries are
// kept in the index — Auditable.History / Rollback continue to work.
//
// Unlike the vector path in supersedeNeighbours, this channel does
// not require an embedder or a vector; matching is exact-string on
// the MetaSlotKey metadata field. Backends without composite filter
// pushdown still hit a single equality predicate which all in-tree
// backends support.
//
// supersede counter accounting: the metric is incremented inside the
// Upsert-success branch only, so callers reading
// supersede_total{channel="slot"} get the count of entries actually
// re-tagged (not the count of candidates returned by List, which
// includes the new entry itself plus any candidates already pointing
// at a previous superseder).
func (m *lt) supersedeBySlot(
	ctx context.Context, scope Scope, newID string,
	fact ExtractedFact, now time.Time,
) {
	ns := NamespaceFor(scope)
	slotKey := fact.Subject + slotDelimiter + fact.Predicate
	resp, err := m.idx.List(ctx, ns, retrieval.ListRequest{
		Filter: MergeFilters(
			AgentRecallFilter(scope),
			retrieval.Filter{Eq: map[string]any{MetaSlotKey: slotKey}},
		),
		PageSize: 64,
	})
	if err != nil || resp == nil {
		if err != nil {
			m.log("ltm: slot supersede list failed: %v", err)
		}
		return
	}
	for _, d := range resp.Items {
		if d.ID == newID {
			continue
		}
		if d.Metadata != nil {
			if v, ok := d.Metadata[MetaSupersededBy].(string); ok && v != "" {
				continue
			}
		}
		updated := d
		if updated.Metadata == nil {
			updated.Metadata = map[string]any{}
		}
		updated.Metadata[MetaSupersededBy] = newID
		updated.Metadata[MetaSupersededAt] = now.UnixMilli()
		if err := m.idx.Upsert(ctx, ns, []retrieval.Doc{updated}); err != nil {
			m.log("ltm: slot supersede upsert failed for %q: %v", d.ID, err)
			continue
		}
		supersedeTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("channel", "slot")))
	}
}

// isEpisodicFact reports whether the extractor flagged the fact as
// an append-only timeline record. The signal is the explicit
// ExtractedFact.Episodic boolean, a first-class architectural field;
// parseFactsJSON also infers it from the legacy "episodic" / "events"
// category strings so extractors that have not been migrated yet
// still benefit from the supersede guard. See godoc on
// supersedeNeighbours for the architectural rationale.
func isEpisodicFact(f ExtractedFact) bool {
	return f.Episodic
}

// slotEligible reports whether a fact qualifies for the deterministic
// slot supersede channel. The contract MUST stay in sync with the
// metadata-writing branch in upsertFacts so a fact that gets a
// slot_key written is exactly the fact that supersedeBySlot will
// later look up.
func slotEligible(f ExtractedFact) bool {
	if f.Subject == "" || f.Predicate == "" {
		return false
	}
	if strings.Contains(f.Subject, slotDelimiter) {
		return false
	}
	if strings.Contains(f.Predicate, slotDelimiter) {
		return false
	}
	return true
}

func setEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
