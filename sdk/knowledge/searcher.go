package knowledge

import "context"

// Retriever produces a candidate set for a Query. Implementations are
// stateless with respect to the Query; all state lives in the underlying
// repos.
type Retriever interface {
	// Name returns a stable identifier used by Rankers (e.g. "bm25").
	Name() string
	// Recall fetches up to q.TopK candidates for q.
	Recall(ctx context.Context, q Query) ([]Candidate, error)
}

// Ranker fuses candidates from multiple Retrievers into ordered Hits.
type Ranker interface {
	Rank(candidates []Candidate, q Query) []Hit
}

// SearchEngine routes a Query through Retrievers, then a Ranker.
//
// Behaviour:
//   - LayerDetail queries fan out to chunk-tier Retrievers (BM25/Vector).
//   - Layer{Abstract,Overview} queries fan out to layer-tier Retrievers.
//   - ModeHybrid runs both BM25 and Vector recall paths and lets the
//     Ranker fuse them (RRF by default).
type SearchEngine struct {
	Chunk []Retriever // chunk-tier retrievers (LayerDetail)
	Layer []Retriever // layer-tier retrievers (LayerAbstract / LayerOverview)
	Rank  Ranker
}

// NewSearchEngine assembles a SearchEngine. ranker may be nil; the Service
// will substitute a default RRFRanker.
func NewSearchEngine(chunk, layer []Retriever, ranker Ranker) *SearchEngine {
	if ranker == nil {
		ranker = NewRRFRanker()
	}
	return &SearchEngine{Chunk: chunk, Layer: layer, Rank: ranker}
}

// Search runs the engine for one Query. Validation is the caller's job
// (Service.Search performs it before delegating).
func (e *SearchEngine) Search(ctx context.Context, q Query) (*Result, error) {
	if e == nil {
		return &Result{}, nil
	}
	retrievers := e.Chunk
	if q.Layer == LayerAbstract || q.Layer == LayerOverview {
		retrievers = e.Layer
	}
	var all []Candidate
	for _, r := range retrievers {
		if r == nil {
			continue
		}
		cands, err := r.Recall(ctx, q)
		if err != nil {
			return nil, err
		}
		all = append(all, cands...)
	}
	hits := e.Rank.Rank(all, q)
	return &Result{Hits: hits}, nil
}
