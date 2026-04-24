package pipeline

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/textsearch"
)

// BM25Boost rescores already-recalled hits with a BM25 signal computed
// client-side over Doc.Content; it does NOT widen the candidate set.
//
// Treat BM25 as a ranking boost, not a recall expander: use it after a
// vector recall + (optional) entity boost when the backend cannot push
// down BM25 natively.
//
// Reads: Final / Reranked / Fused, Request.QueryText. Writes: Final.
type BM25Boost struct {
	// Weight is added on top of the existing score as Score += Weight * bm25Norm.
	// bm25Norm is min-max normalized across the candidate slice.
	// Defaults to 0.3.
	Weight float64
}

// Name implements Stage.
func (s BM25Boost) Name() string { return "BM25Boost" }

// Run implements Stage.
func (s BM25Boost) Run(_ context.Context, st *State) error {
	if st.Request == nil || strings.TrimSpace(st.Request.QueryText) == "" {
		return nil
	}
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	w := s.Weight
	if w <= 0 {
		w = 0.3
	}
	tok := textsearch.DetectTokenizer(st.Request.QueryText)
	qTerms := tok.Tokenize(st.Request.QueryText)
	if len(qTerms) == 0 {
		return nil
	}
	docTokens := make([][]string, len(hits))
	totalLen := 0
	for i, h := range hits {
		docTokens[i] = tok.Tokenize(h.Doc.Content)
		totalLen += len(docTokens[i])
	}
	avgLen := 0.0
	if len(hits) > 0 {
		avgLen = float64(totalLen) / float64(len(hits))
	}
	const k1, b = 1.5, 0.75
	df := map[string]int{}
	for _, terms := range docTokens {
		seen := map[string]struct{}{}
		for _, t := range terms {
			seen[t] = struct{}{}
		}
		for t := range seen {
			df[t]++
		}
	}
	N := float64(len(hits))
	scoresBM25 := make([]float64, len(hits))
	maxBM25 := math.Inf(-1)
	minBM25 := math.Inf(+1)
	for i, terms := range docTokens {
		tf := map[string]int{}
		for _, t := range terms {
			tf[t]++
		}
		dl := float64(len(terms))
		denomNorm := 1.0
		if avgLen > 0 {
			denomNorm = 1 - b + b*dl/avgLen
		}
		var s float64
		for _, q := range qTerms {
			n := float64(df[q])
			if n == 0 {
				continue
			}
			idf := safeLog((N - n + 0.5) / (n + 0.5))
			if idf < 0 {
				idf = 0
			}
			f := float64(tf[q])
			if f == 0 {
				continue
			}
			s += idf * (f * (k1 + 1)) / (f + k1*denomNorm)
		}
		scoresBM25[i] = s
		if s > maxBM25 {
			maxBM25 = s
		}
		if s < minBM25 {
			minBM25 = s
		}
	}
	span := maxBM25 - minBM25
	for i := range hits {
		var norm float64
		if span > 0 {
			norm = (scoresBM25[i] - minBM25) / span
		} else if maxBM25 > 0 {
			norm = 1
		}
		hits[i].Score = hits[i].Score + w*norm
		if hits[i].Scores == nil {
			hits[i].Scores = map[string]float64{}
		}
		hits[i].Scores["bm25_boost"] = norm
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	st.Final = hits
	return nil
}

// ScoreThreshold drops hits whose Score falls below Min.
//
// Use a small positive Min (e.g. 0.05–0.1) to keep candidate sets lean
// before downstream rerank/limit stages. Reads/Writes: Final.
type ScoreThreshold struct {
	Min float64
}

// Name implements Stage.
func (s ScoreThreshold) Name() string { return "ScoreThreshold" }

// Run implements Stage.
func (s ScoreThreshold) Run(_ context.Context, st *State) error {
	if s.Min <= 0 {
		return nil
	}
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	out := hits[:0]
	for _, h := range hits {
		if h.Score >= s.Min {
			out = append(out, h)
		}
	}
	st.Final = out
	return nil
}

// SupersededDecay penalizes hits whose Doc.Metadata["superseded_by"] is set,
// implementing the read-side half of recall's soft-merge contract: when
// recall.Memory.Save observes that a new fact's entity set + vector
// cosine matches an older entry, it stamps the older row with
// superseded_by=<new_id> instead of deleting it. This stage damps the
// older row at retrieval time so newer revisions float to the top
// while the audit trail stays intact.
//
// Score is multiplied by Factor (default 0.3); set Factor close to 1
// to make supersedence purely informational (no ranking impact).
//
// Reads/Writes: Final.
type SupersededDecay struct {
	Factor float64
}

// Name implements Stage.
func (s SupersededDecay) Name() string { return "SupersededDecay" }

// Run implements Stage.
func (s SupersededDecay) Run(_ context.Context, st *State) error {
	hits := pickFinalish(st)
	if len(hits) == 0 {
		return nil
	}
	f := s.Factor
	if f <= 0 {
		f = 0.3
	}
	for i, h := range hits {
		if h.Doc.Metadata == nil {
			continue
		}
		v, ok := h.Doc.Metadata["superseded_by"]
		if !ok {
			continue
		}
		if str, ok := v.(string); !ok || str == "" {
			continue
		}
		hits[i].Score = h.Score * f
		if hits[i].Scores == nil {
			hits[i].Scores = map[string]float64{}
		}
		hits[i].Scores["superseded_decay"] = f
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	st.Final = hits
	return nil
}

func safeLog(x float64) float64 {
	if x <= 0 {
		return 0
	}
	return math.Log(x)
}
