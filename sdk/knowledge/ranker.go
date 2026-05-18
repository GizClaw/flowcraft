package knowledge

import (
	"sort"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/scoring"
)

// DefaultRRFK is the conventional fusion constant for reciprocal-rank fusion.
// Smaller K weights the head of each ranked list more heavily.
const DefaultRRFK = 60

// RRFRanker performs reciprocal-rank fusion across candidates grouped by
// Candidate.Source. K defaults to DefaultRRFK when zero.
type RRFRanker struct {
	K int
}

// NewRRFRanker constructs an RRFRanker with sensible defaults.
func NewRRFRanker() *RRFRanker { return &RRFRanker{K: DefaultRRFK} }

// Rank fuses candidates and truncates to q.TopK (zero means "no limit").
//
// Threshold filtering is applied AFTER fusion using the fused score:
// candidates whose fused score is below q.Threshold are dropped. When
// q.Threshold is zero, no filtering is applied.
func (r *RRFRanker) Rank(candidates []Candidate, q Query) []Hit {
	if len(candidates) == 0 {
		return nil
	}
	k := r.K
	if k <= 0 {
		k = DefaultRRFK
	}

	groups := make(map[string][]Candidate)
	order := make([]string, 0)
	for _, c := range candidates {
		if _, ok := groups[c.Source]; !ok {
			order = append(order, c.Source)
		}
		groups[c.Source] = append(groups[c.Source], c)
	}
	for _, src := range order {
		g := groups[src]
		sort.SliceStable(g, func(i, j int) bool { return g[i].Hit.Score > g[j].Hit.Score })
		groups[src] = g
	}

	firstHit := make(map[string]Hit)
	lanes := make([][]retrieval.Hit, 0, len(order))
	for _, src := range order {
		lane := make([]retrieval.Hit, 0, len(groups[src]))
		for rank, c := range groups[src] {
			_ = rank
			key := candidateKey(c)
			if _, ok := firstHit[key]; !ok {
				firstHit[key] = c.Hit
			}
			lane = append(lane, retrieval.Hit{
				Doc:   retrieval.Doc{ID: key},
				Score: c.Hit.Score,
			})
		}
		lanes = append(lanes, lane)
	}

	fused := scoring.RRF(lanes, float64(k))
	out := make([]Hit, 0, len(fused))
	for _, fh := range fused {
		if q.Threshold > 0 && fh.Score < q.Threshold {
			continue
		}
		h := firstHit[fh.Doc.ID]
		h.Score = fh.Score
		out = append(out, h)
	}
	if q.TopK > 0 && len(out) > q.TopK {
		out = out[:q.TopK]
	}
	return out
}

// candidateKey identifies a unique result across retriever sources.
func candidateKey(c Candidate) string {
	h := c.Hit
	return h.DatasetID + "\x00" + h.DocName + "\x00" + string(h.Layer) + "\x00" + itoa(h.ChunkIndex)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// FuseHits is a free-function form of RRF fusion, useful for tooling that
// already has per-retriever Hit lists. It is the building block used by
// RRFRanker.Rank.
func FuseHits(perRetriever [][]Hit, k int) []Hit {
	if k <= 0 {
		k = DefaultRRFK
	}
	firstHit := make(map[string]Hit)
	lanes := make([][]retrieval.Hit, 0, len(perRetriever))
	for _, list := range perRetriever {
		lane := make([]retrieval.Hit, 0, len(list))
		for rank, h := range list {
			_ = rank
			key := h.DatasetID + "\x00" + h.DocName + "\x00" + string(h.Layer) + "\x00" + itoa(h.ChunkIndex)
			if _, ok := firstHit[key]; !ok {
				firstHit[key] = h
			}
			lane = append(lane, retrieval.Hit{Doc: retrieval.Doc{ID: key}, Score: h.Score})
		}
		lanes = append(lanes, lane)
	}
	fused := scoring.RRF(lanes, float64(k))
	out := make([]Hit, 0, len(fused))
	for _, fh := range fused {
		h := firstHit[fh.Doc.ID]
		h.Score = fh.Score
		out = append(out, h)
	}
	return out
}
