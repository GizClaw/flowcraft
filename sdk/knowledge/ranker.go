package knowledge

import "sort"

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

	type fused struct {
		hit   Hit
		score float64
	}
	merged := make(map[string]*fused)
	keyOrder := make([]string, 0)
	for _, src := range order {
		for rank, c := range groups[src] {
			key := candidateKey(c)
			contrib := 1.0 / float64(k+rank+1)
			if existing, ok := merged[key]; ok {
				existing.score += contrib
				continue
			}
			merged[key] = &fused{hit: c.Hit, score: contrib}
			keyOrder = append(keyOrder, key)
		}
	}

	out := make([]Hit, 0, len(keyOrder))
	for _, key := range keyOrder {
		f := merged[key]
		if q.Threshold > 0 && f.score < q.Threshold {
			continue
		}
		h := f.hit
		h.Score = f.score
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
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
	type fused struct {
		hit   Hit
		score float64
	}
	merged := make(map[string]*fused)
	order := make([]string, 0)
	for _, list := range perRetriever {
		for rank, h := range list {
			key := h.DatasetID + "\x00" + h.DocName + "\x00" + string(h.Layer) + "\x00" + itoa(h.ChunkIndex)
			contrib := 1.0 / float64(k+rank+1)
			if existing, ok := merged[key]; ok {
				existing.score += contrib
				continue
			}
			merged[key] = &fused{hit: h, score: contrib}
			order = append(order, key)
		}
	}
	out := make([]Hit, 0, len(order))
	for _, key := range order {
		f := merged[key]
		h := f.hit
		h.Score = f.score
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
