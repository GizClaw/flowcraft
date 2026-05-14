package pipeline

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// RetrieveMode is one source for MultiRetrieve / Retrieve.
//
// The string value of each mode MUST match the corresponding
// [retrieval.LaneKey] constant — they share the same wire format so that
// SearchExecution.Lanes produced by this package can be keyed and
// rendered uniformly across backends.
type RetrieveMode string

const (
	ModeBM25   RetrieveMode = RetrieveMode(retrieval.LaneBM25)
	ModeVector RetrieveMode = RetrieveMode(retrieval.LaneVector)
	ModeSparse RetrieveMode = RetrieveMode(retrieval.LaneSparse)
	ModeEntity RetrieveMode = RetrieveMode(retrieval.LaneEntity)
)

// RetrieveSpec controls one recall lane.
type RetrieveSpec struct {
	Mode   RetrieveMode
	TopK   int
	Filter retrieval.Filter

	// MinSelectivity is an entity-lane-only gate that skips the
	// lane when no query atom is selective enough — i.e. when every
	// atom appears in MinSelectivity * N or more docs (where N is
	// the universe size under Filter). 0 disables the gate.
	//
	// Motivation: the entity recall lane on its own does
	// "filter by entity overlap → rank by IDF". When all query
	// atoms are universal ("tuesday", "morning", "favorite"),
	// IDF saturates near zero across all candidates and the lane
	// degrades to "return ~N docs in undefined order", whose
	// rank vote in RRF then displaces vector recall's precision
	// picks. Gating on selectivity collapses this case to "lane
	// returns nothing" so the fused result falls back to vector +
	// BM25 alone.
	//
	// Default in pipeline.LTM (when WithMultiRecall is enabled) is
	// 0.1, meaning the lane fires only when at least one query
	// atom matches fewer than 10% of the namespace's docs.
	MinSelectivity float64
}

// Retrieve runs one recall lane and stores hits in Recalls[name]
// . Reads: Request.*, QueryEntities. Writes: Recalls[Name], RecallTimings[Name].
type Retrieve struct {
	Lane string
	Spec RetrieveSpec
}

// Name implements Stage.
func (s Retrieve) Name() string { return "Retrieve(" + s.Lane + ")" }

// Run implements Stage.
func (s Retrieve) Run(ctx context.Context, st *State) error {
	if st.Recalls == nil {
		st.Recalls = make(map[string][]retrieval.Hit)
	}
	if st.RecallTimings == nil {
		st.RecallTimings = make(map[string]time.Duration)
	}
	t0 := time.Now()
	hits, err := runOneRecall(ctx, st, s.Lane, s.Spec)
	st.RecallTimings[s.Lane] = time.Since(t0)
	if err != nil {
		return err
	}
	st.Recalls[s.Lane] = hits
	return nil
}

// MultiRetrieve runs multiple recall lanes concurrently
// . Reads: Request.*, QueryEntities. Writes: Recalls, RecallTimings.
//
// Error policy (v0): the first lane error short-circuits the stage and is
// returned to the pipeline; lanes that already produced results are
// preserved in State.Recalls / State.RecallTimings, so partial telemetry
// remains observable, but the pipeline as a whole stops. A future
// "tolerant" mode that downgrades lane failures to entries on a
// State.RecallErrors map is tracked under the broader recall-degrade RFC;
// callers that need it today should run lanes via separate Retrieve
// stages and tolerate errors at the pipeline level.
type MultiRetrieve map[string]RetrieveSpec

// Name implements Stage.
func (s MultiRetrieve) Name() string { return "MultiRetrieve" }

// Run implements Stage.
func (s MultiRetrieve) Run(ctx context.Context, st *State) error {
	if len(s) == 0 {
		return nil
	}
	if st.Recalls == nil {
		st.Recalls = make(map[string][]retrieval.Hit)
	}
	if st.RecallTimings == nil {
		st.RecallTimings = make(map[string]time.Duration, len(s))
	}
	type res struct {
		name string
		hits []retrieval.Hit
		took time.Duration
		err  error
	}
	ch := make(chan res, len(s))
	var wg sync.WaitGroup
	for name, spec := range s {
		wg.Add(1)
		go func(name string, spec RetrieveSpec) {
			defer wg.Done()
			t0 := time.Now()
			hits, err := runOneRecall(ctx, st, name, spec)
			ch <- res{name: name, hits: hits, took: time.Since(t0), err: err}
		}(name, spec)
	}
	go func() { wg.Wait(); close(ch) }()
	for r := range ch {
		// Record timing even on lane error so the dashboard can show
		// "this lane was slow AND failing" instead of dropping the
		// sample entirely.
		st.RecallTimings[r.name] = r.took
		if r.err != nil {
			return r.err
		}
		st.Recalls[r.name] = r.hits
	}
	return nil
}

func runOneRecall(ctx context.Context, st *State, _ string, spec RetrieveSpec) ([]retrieval.Hit, error) {
	if st.Request == nil || st.Index == nil {
		return nil, nil
	}
	req := *st.Request
	req.TopK = spec.TopK
	req.HybridMode = ""
	if !filterIsZero(spec.Filter) {
		req.Filter = mergeFilter(req.Filter, spec.Filter)
	}
	switch spec.Mode {
	case ModeBM25:
		req.QueryVector = nil
		req.SparseVec = nil
		if req.QueryText == "" {
			return nil, nil
		}
	case ModeVector:
		req.QueryText = ""
		req.SparseVec = nil
		if len(req.QueryVector) == 0 {
			return nil, nil
		}
	case ModeSparse:
		req.QueryText = ""
		req.QueryVector = nil
		if len(req.SparseVec) == 0 {
			return nil, nil
		}
	case ModeEntity:
		return runEntityRecall(ctx, st, req, spec)
	}
	resp, err := st.Index.Search(ctx, st.Namespace, req)
	if err != nil || resp == nil {
		return nil, err
	}
	out := make([]retrieval.Hit, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		switch spec.Mode {
		case ModeBM25:
			if h.Scores != nil && h.Scores["bm25"] <= 0 {
				continue
			}
		case ModeVector:
			if h.Scores != nil && h.Scores["cos"] <= 0 {
				continue
			}
		case ModeSparse:
			if h.Scores != nil && h.Scores["sparse"] <= 0 {
				continue
			}
		}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// entityCandidatePageSize caps how many entity-matching docs we
// rerank client-side per recall. Raw overlap-count scoring was
// indifferent to which docs the backend returned first; IDF scoring
// is not, because the highest-IDF doc may sit deep in the
// timestamp-ordered List page. Picking a wide page-size avoids
// silently dropping high-signal candidates to the page boundary.
// The actual cost on the in-memory backend is one map scan; for
// production backends this becomes "scan up to N candidates per
// recall" which is bounded by the per-namespace fact count.
const entityCandidatePageSize = 1000

// runEntityRecall implements the entity-lane recall with corpus-IDF
// weighting. The previous overlap-count scorer behaved well only when
// the entity lane was effectively dead (case-sensitive ContainsAny
// vs lowercased query atoms never fired in production) — once
// sdk/recall.NormalizeEntities atomised stored phrases at ingest and
// the filter started firing, the lane immediately surfaced high-
// frequency calendar atoms ("tuesday", "morning", "meeting") as
// top-ranked matches, polluting RRF fusion and dropping LoCoMo
// qa.judge by ~21 pp.
//
// IDF weighting fixes this by valuing each query atom by how rare it
// is in the namespace under the request's existing scope filter:
//
//	idf(atom) = log( (N + 1) / (df(atom) + 1) )
//
// where N is the total docs visible to the request filter and df is
// the count of docs whose `entities` metadata contains `atom`. The
// +1 smoothing avoids divide-by-zero and keeps an atom that appears
// in every doc at exactly zero weight rather than negative.
//
// Cost: O(len(QueryEntities) + 1) cheap List(PageSize=1) calls to
// build the IDF table, plus one List(PageSize=entityCandidatePageSize)
// to retrieve candidates. On the in-memory backend the cheap calls
// touch only the term-frequency aggregate (Total field) and skip
// page materialisation.
func runEntityRecall(ctx context.Context, st *State, req retrieval.SearchRequest, spec RetrieveSpec) ([]retrieval.Hit, error) {
	if len(st.QueryEntities) == 0 {
		return nil, nil
	}
	// 1. Universe size (under the request's pre-existing scope filter).
	nResp, err := st.Index.List(ctx, st.Namespace, retrieval.ListRequest{
		Filter:   req.Filter,
		PageSize: 1,
	})
	if err != nil {
		return nil, err
	}
	N := int64(0)
	if nResp != nil {
		N = nResp.Total
	}
	if N < 1 {
		N = 1
	}

	// 2. Per-atom df. We resolve atoms case-insensitively against
	//    stored entities by lowercasing both sides; QueryEntities is
	//    already lowercase courtesy of dedupStringsLower in
	//    EntityExtract, so we only need to normalise the query side
	//    on the way in.
	idfs := make(map[string]float64, len(st.QueryEntities))
	dfs := make(map[string]int64, len(st.QueryEntities))
	for _, e := range st.QueryEntities {
		atom := strings.ToLower(strings.TrimSpace(e))
		if atom == "" {
			continue
		}
		if _, seen := idfs[atom]; seen {
			continue
		}
		dfFilter := mergeFilter(req.Filter, retrieval.Filter{
			ContainsAny: map[string][]any{"entities": {atom}},
		})
		dfResp, err := st.Index.List(ctx, st.Namespace, retrieval.ListRequest{
			Filter:   dfFilter,
			PageSize: 1,
		})
		if err != nil {
			continue
		}
		df := int64(0)
		if dfResp != nil {
			df = dfResp.Total
		}
		dfs[atom] = df
		idfs[atom] = math.Log(float64(N+1) / float64(df+1))
	}
	if len(idfs) == 0 {
		return nil, nil
	}

	// 2a. Selectivity gate. Skip the lane when no query atom is
	//     rare enough to discriminate within the namespace — see
	//     RetrieveSpec.MinSelectivity for the rationale. The
	//     threshold is an *upper bound* on df: an atom is "selective"
	//     iff it matches strictly fewer than MinSelectivity * N docs.
	if spec.MinSelectivity > 0 {
		// floor of MinSelectivity*N, with a +0 floor of 1 so even
		// the smallest namespace can have a "rare" atom.
		threshold := int64(float64(N) * spec.MinSelectivity)
		if threshold < 1 {
			threshold = 1
		}
		hasRare := false
		for _, df := range dfs {
			// df==0 means the atom has no matching docs at all, so
			// the lane will return nothing regardless — treat as
			// not-rare to keep the early-skip path consistent.
			if df > 0 && df < threshold {
				hasRare = true
				break
			}
		}
		if !hasRare {
			return nil, nil
		}
	}

	// 3. Materialise candidates (union of any-atom-match) and rank
	//    client-side by IDF-weighted overlap.
	atomsAny := make([]any, 0, len(idfs))
	for a := range idfs {
		atomsAny = append(atomsAny, a)
	}
	entityFilter := retrieval.Filter{ContainsAny: map[string][]any{"entities": atomsAny}}
	mergedFilter := mergeFilter(req.Filter, entityFilter)
	listResp, err := st.Index.List(ctx, st.Namespace, retrieval.ListRequest{
		Filter:   mergedFilter,
		PageSize: entityCandidatePageSize,
	})
	if err != nil || listResp == nil {
		return nil, err
	}

	hits := make([]retrieval.Hit, 0, len(listResp.Items))
	for _, d := range listResp.Items {
		seen := make(map[string]struct{})
		score := 0.0
		for _, raw := range docEntities(d) {
			a := strings.ToLower(strings.TrimSpace(raw))
			if a == "" {
				continue
			}
			if _, dup := seen[a]; dup {
				continue
			}
			seen[a] = struct{}{}
			if w, ok := idfs[a]; ok && w > 0 {
				score += w
			}
		}
		if score <= 0 {
			// Filter matched (so at least one query atom is in this
			// doc's entities) but every matching atom had IDF ≤ 0,
			// i.e. appears in every doc. Skip — keeping it at zero
			// would still let RRF pull it in via rank, which is the
			// pre-IDF failure mode we are fixing.
			continue
		}
		hits = append(hits, retrieval.Hit{
			Doc:    d,
			Score:  score,
			Scores: map[string]float64{"entity_idf": score},
		})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if spec.TopK > 0 && len(hits) > spec.TopK {
		hits = hits[:spec.TopK]
	}
	return hits, nil
}

// liftRecall promotes Recalls[Lane] into Final, letting subsequent stages
// treat it as the canonical ranked list. Used by single-lane recall recipes
// (e.g. vector-first LTM) that skip the fusion step.
//
// Reads: Recalls[Lane]. Writes: Final.
type liftRecall struct {
	Lane string
}

// Name implements Stage.
func (s liftRecall) Name() string { return "Lift(" + s.Lane + ")" }

// Run implements Stage.
//
// Pipeline.Run already breaks out of the stage loop when ShortCircuit is set,
// so liftRecall does not need to re-check it; reaching Run at all means the
// pipeline still wants the lift to happen.
func (s liftRecall) Run(_ context.Context, st *State) error {
	if st.Recalls == nil {
		return nil
	}
	hits := st.Recalls[s.Lane]
	if len(hits) == 0 {
		return nil
	}
	cp := make([]retrieval.Hit, len(hits))
	copy(cp, hits)
	st.Final = cp
	return nil
}

func filterIsZero(f retrieval.Filter) bool {
	return len(f.And) == 0 && len(f.Or) == 0 && f.Not == nil &&
		len(f.Eq) == 0 && len(f.Neq) == 0 && len(f.In) == 0 && len(f.NotIn) == 0 &&
		len(f.Range) == 0 && len(f.Exists) == 0 && len(f.Missing) == 0 && len(f.Match) == 0 &&
		len(f.Contains) == 0 && len(f.IContains) == 0 && len(f.ContainsAny) == 0 && len(f.ContainsAll) == 0
}

func mergeFilter(a, b retrieval.Filter) retrieval.Filter {
	if filterIsZero(a) {
		return b
	}
	if filterIsZero(b) {
		return a
	}
	return retrieval.Filter{And: []retrieval.Filter{a, b}}
}
