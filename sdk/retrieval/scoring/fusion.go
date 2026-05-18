package scoring

import (
	"math"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// DefaultRRFK is the default damping constant for [RRF]. The value
// 60 is the commonly cited choice from Cormack et al. (2009) and is
// the same default used by sdk/retrieval/pipeline.RRFFusion.
const DefaultRRFK = 60.0

// RRF (Reciprocal Rank Fusion) merges several ranked hit lists into a
// single ranked list. For every (list, position) pair a hit
// contributes 1 / (k + rank+1) to its document's total score,
// independent of the original score scales. This makes RRF
// hyperparameter-light and robust against poorly normalized lanes,
// which is why it is the default fusion in production retrieval
// pipelines (Vespa, Elasticsearch, etc.).
//
// The returned slice is sorted by descending RRF score; ties are
// broken stably by insertion order. Each Hit's Score is replaced by
// the RRF score and the per-doc Scores map gets a "rrf" entry. If
// the same doc appears in multiple lists, its returned Hit copy
// carries the highest-original-score variant (so chunk text /
// metadata picked from the strongest lane survives the merge).
//
// k <= 0 falls back to [DefaultRRFK].
func RRF(rankedLists [][]retrieval.Hit, k float64) []retrieval.Hit {
	if k <= 0 {
		k = DefaultRRFK
	}
	if len(rankedLists) == 0 {
		return nil
	}
	scores := map[string]float64{}
	docs := map[string]retrieval.Hit{}
	firstSeen := map[string]int{}
	seq := 0
	for _, hits := range rankedLists {
		for rank, h := range hits {
			if _, ok := firstSeen[h.Doc.ID]; !ok {
				firstSeen[h.Doc.ID] = seq
				seq++
			}
			scores[h.Doc.ID] += 1.0 / (k + float64(rank+1))
			if cur, ok := docs[h.Doc.ID]; !ok || h.Score > cur.Score {
				docs[h.Doc.ID] = retrieval.CloneHit(h)
			}
		}
	}
	out := make([]retrieval.Hit, 0, len(scores))
	for id, s := range scores {
		h := retrieval.CloneHit(docs[id])
		h.Score = s
		if h.Scores == nil {
			h.Scores = map[string]float64{}
		}
		h.Scores["rrf"] = s
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if firstSeen[out[i].Doc.ID] != firstSeen[out[j].Doc.ID] {
			return firstSeen[out[i].Doc.ID] < firstSeen[out[j].Doc.ID]
		}
		return out[i].Doc.ID < out[j].Doc.ID
	})
	return out
}

// WeightedFusion merges several lanes via per-lane weights after
// independent min-max normalization within each lane. lanes maps a
// lane label (e.g. "bm25", "vector") to its ranked hit list;
// weights maps the same labels to a multiplier. Missing weights
// default to 1.0; missing lanes are skipped.
//
// For each lane:
//
//   - scores are min-max normalized to [0,1]; a degenerate lane
//     (span == 0) collapses to 1.0 if any score is positive,
//     otherwise 0.
//   - the normalized score is multiplied by the lane weight and
//     accumulated per doc.
//
// As with [RRF], when the same doc appears across lanes the returned
// Hit copy carries the highest-original-score variant.
func WeightedFusion(lanes map[string][]retrieval.Hit, weights map[string]float64) []retrieval.Hit {
	if len(lanes) == 0 {
		return nil
	}
	totals := map[string]float64{}
	docs := map[string]retrieval.Hit{}
	firstSeen := map[string]int{}
	seq := 0
	laneNames := make([]string, 0, len(lanes))
	for lane := range lanes {
		laneNames = append(laneNames, lane)
	}
	sort.Strings(laneNames)
	for _, lane := range laneNames {
		hits := lanes[lane]
		w := 1.0
		if v, ok := weights[lane]; ok {
			w = v
		}
		var min, max = math.MaxFloat64, -math.MaxFloat64
		for _, h := range hits {
			if h.Score < min {
				min = h.Score
			}
			if h.Score > max {
				max = h.Score
			}
		}
		span := max - min
		for _, h := range hits {
			if _, ok := firstSeen[h.Doc.ID]; !ok {
				firstSeen[h.Doc.ID] = seq
				seq++
			}
			n := 0.0
			if span > 0 {
				n = (h.Score - min) / span
			} else if max > 0 {
				n = 1
			}
			totals[h.Doc.ID] += w * n
			if cur, ok := docs[h.Doc.ID]; !ok || h.Score > cur.Score {
				docs[h.Doc.ID] = retrieval.CloneHit(h)
			}
		}
	}
	out := make([]retrieval.Hit, 0, len(totals))
	for id, t := range totals {
		h := retrieval.CloneHit(docs[id])
		h.Score = t
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if firstSeen[out[i].Doc.ID] != firstSeen[out[j].Doc.ID] {
			return firstSeen[out[i].Doc.ID] < firstSeen[out[j].Doc.ID]
		}
		return out[i].Doc.ID < out[j].Doc.ID
	})
	return out
}
