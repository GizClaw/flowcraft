package recall

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
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
		Filter:   retrieval.Filter{In: map[string][]any{"content_hash": in}},
		PageSize: len(hashes),
	})
	if err != nil || resp == nil {
		return out, err
	}
	for _, d := range resp.Items {
		if d.Metadata == nil {
			continue
		}
		if v, ok := d.Metadata["content_hash"].(string); ok {
			out[v] = d.ID
		}
	}
	return out, nil
}

// supersedeNeighbours marks older entries whose entity set matches the new
// fact AND whose vector cosine ≥ SoftMergeCosineMin with metadata.superseded_by.
// Old entries are NOT deleted (history preserved for Auditable.History /
// Rollback); retrieval-time damping is the responsibility of
// pipeline.SupersededDecay, which multiplies the score of any hit
// carrying superseded_by by its Factor (default 0.3).
func (m *lt) supersedeNeighbours(
	ctx context.Context, scope Scope, newID string,
	fact ExtractedFact, vec []float32, now time.Time,
) {
	if !m.cfg.softMerge || m.cfg.embedder == nil || len(vec) == 0 {
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
			if v, ok := h.Doc.Metadata["superseded_by"].(string); ok && v != "" {
				continue
			}
		}
		updated := h.Doc
		if updated.Metadata == nil {
			updated.Metadata = map[string]any{}
		}
		updated.Metadata["superseded_by"] = newID
		updated.Metadata["superseded_at"] = now.UnixMilli()
		_ = m.idx.Upsert(ctx, ns, []retrieval.Doc{updated})
	}
}

func docEntities(d retrieval.Doc) []string {
	if d.Metadata == nil {
		return nil
	}
	v, ok := d.Metadata["entities"]
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
