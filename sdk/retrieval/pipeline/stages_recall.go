package pipeline

import (
	"context"
	"sort"
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
		if len(st.QueryEntities) == 0 {
			return nil, nil
		}
		entities := make([]any, 0, len(st.QueryEntities))
		for _, e := range st.QueryEntities {
			entities = append(entities, e)
		}
		entityFilter := retrieval.Filter{ContainsAny: map[string][]any{"entities": entities}}
		mergedFilter := mergeFilter(req.Filter, entityFilter)
		listResp, err := st.Index.List(ctx, st.Namespace, retrieval.ListRequest{
			Filter:   mergedFilter,
			PageSize: spec.TopK,
		})
		if err != nil || listResp == nil {
			return nil, err
		}
		// Score the entity lane by overlap count so downstream fusion
		// stages (WeightedFusion / RRF) have a real signal to work
		// with. Without this every entity hit looks identical and the
		// lane behaves as a binary filter.
		qSet := make(map[string]struct{}, len(st.QueryEntities))
		for _, e := range st.QueryEntities {
			qSet[e] = struct{}{}
		}
		hits := make([]retrieval.Hit, 0, len(listResp.Items))
		for _, d := range listResp.Items {
			overlap := 0
			for _, e := range docEntities(d) {
				if _, ok := qSet[e]; ok {
					overlap++
				}
			}
			score := float64(overlap)
			if score == 0 {
				score = 1 // matched the filter but no exact normalised hit
			}
			hits = append(hits, retrieval.Hit{
				Doc:    d,
				Score:  score,
				Scores: map[string]float64{"entity": score},
			})
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		return hits, nil
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
