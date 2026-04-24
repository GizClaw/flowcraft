package pipeline

import (
	"context"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// wellKnownLaneOrder pins the surface order of the LaneKeys defined in
// sdk/retrieval/explain.go so that pipelines emitting the same set of lanes
// produce a deterministic SearchExecution.Lanes slice across runs.
//
// Unknown lane keys (e.g. backend-specific "pgvector.hybrid") fall through
// to lexical order behind the well-known ones; this keeps cross-backend
// dashboards stable while still allowing custom lanes.
var wellKnownLaneOrder = map[retrieval.LaneKey]int{
	retrieval.LaneBM25:       0,
	retrieval.LaneVector:     1,
	retrieval.LaneSparse:     2,
	retrieval.LaneEntity:     3,
	retrieval.LaneHybrid:     4,
	retrieval.LaneFusion:     5,
	retrieval.LaneRerank:     6,
	retrieval.LanePostFilter: 7,
}

func sortedLaneNames(recalls map[string][]retrieval.Hit) []string {
	names := make([]string, 0, len(recalls))
	for k := range recalls {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		oi, iKnown := wellKnownLaneOrder[retrieval.LaneKey(names[i])]
		oj, jKnown := wellKnownLaneOrder[retrieval.LaneKey(names[j])]
		switch {
		case iKnown && jKnown:
			return oi < oj
		case iKnown != jKnown:
			return iKnown
		default:
			return names[i] < names[j]
		}
	})
	return names
}

// Stage is one step of a retrieval pipeline.
type Stage interface {
	Name() string
	Run(ctx context.Context, state *State) error
}

// StageTrace records latency and outcome of one stage execution.
type StageTrace struct {
	Stage    string
	Duration time.Duration
	Err      error
	HitsIn   int
	HitsOut  int
}

// State is the shared workspace across stages.
//
// Stages MUST document which fields they read/write in their Name()/godoc.
// Adding new fields requires an RFC amendment (anti-bag-bloat constraint).
type State struct {
	Index     retrieval.Index
	Namespace string
	Request   *retrieval.SearchRequest

	QueryVariants []string
	QueryVector   []float32
	QueryEntities []string
	Recalls       map[string][]retrieval.Hit
	// RecallTimings is keyed identically to Recalls and records how long
	// the corresponding recall lane took. Populated by Retrieve /
	// MultiRetrieve; consumed by pipeline.Run to fill LaneResult.Took.
	// Zero is a valid value (lane skipped or no-op).
	RecallTimings map[string]time.Duration
	Fused         []retrieval.Hit
	Reranked      []retrieval.Hit
	Final         []retrieval.Hit

	ShortCircuit bool

	// HybridExecution is set by HybridShortCircuit when a backend honours
	// Debug on its native hybrid path. Pipeline.Run merges these lanes /
	// stages into SearchResponse.Execution so callers see one consistent
	// explanation regardless of whether the request was short-circuited.
	HybridExecution *retrieval.SearchExecution

	Trace []StageTrace
}

// Pipeline is a linear ordered set of Stages (v0: no DAG).
type Pipeline struct {
	stages []Stage
}

// New constructs a Pipeline.
func New(stages ...Stage) *Pipeline {
	return &Pipeline{stages: stages}
}

// Stages returns a copy of the configured stages (introspection / testing).
func (p *Pipeline) Stages() []Stage {
	out := make([]Stage, len(p.stages))
	copy(out, p.stages)
	return out
}

// Run executes each stage; on ShortCircuit the rest are skipped, but cleanup
// stages with idempotent Run can still be appended explicitly.
//
// Telemetry: each stage gets a child span retrieval.stage.<name>; any
// Stage.Run error is recorded on its own span. The parent caller (e.g.
// memory.recall.recall) supplies the enclosing span, so the trace tree
// directly answers "which retrieval stage was the slow one".
func (p *Pipeline) Run(ctx context.Context, idx retrieval.Index, namespace string, req retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	st := &State{
		Index:     idx,
		Namespace: namespace,
		Request:   &req,
		Recalls:   make(map[string][]retrieval.Hit),
	}
	tracer := telemetry.Tracer()
	overall := time.Now()
	for _, s := range p.stages {
		if st.ShortCircuit {
			break
		}
		stageCtx, span := tracer.Start(ctx, "retrieval.stage."+s.Name())
		t0 := time.Now()
		hitsIn := currentHitCount(st)
		err := s.Run(stageCtx, st)
		dur := time.Since(t0)
		hitsOut := currentHitCount(st)
		span.SetAttributes(attribute.Float64("duration_seconds", dur.Seconds()))
		if err != nil {
			span.RecordError(err)
		}
		span.End()
		st.Trace = append(st.Trace, StageTrace{
			Stage:    s.Name(),
			Duration: dur,
			Err:      err,
			HitsIn:   hitsIn,
			HitsOut:  hitsOut,
		})
		if err != nil {
			return nil, err
		}
	}
	hits := st.Final
	if hits == nil {
		hits = st.Reranked
	}
	if hits == nil {
		hits = st.Fused
	}
	resp := &retrieval.SearchResponse{Hits: hits, Took: time.Since(overall)}

	debug := req.Debug
	if req.ReturnRaw {
		debug.IncludeLanes = true
	}

	if debug.IncludeLanes || debug.IncludeStages {
		exec := &retrieval.SearchExecution{}
		if debug.IncludeLanes && len(st.Recalls) > 0 {
			names := sortedLaneNames(st.Recalls)
			exec.Lanes = make([]retrieval.LaneResult, 0, len(names))
			for _, name := range names {
				laneHits := st.Recalls[name]
				cp := make([]retrieval.Hit, len(laneHits))
				copy(cp, laneHits)
				exec.Lanes = append(exec.Lanes, retrieval.LaneResult{
					Key:  retrieval.LaneKey(name),
					Hits: cp,
					Took: st.RecallTimings[name],
				})
			}
		}
		// Merge any explanation produced by a short-circuited hybrid backend
		// so the caller observes a single Execution surface regardless of
		// whether the pipeline ran its own lanes.
		if debug.IncludeLanes && st.HybridExecution != nil {
			for _, lane := range st.HybridExecution.Lanes {
				cp := make([]retrieval.Hit, len(lane.Hits))
				copy(cp, lane.Hits)
				exec.Lanes = append(exec.Lanes, retrieval.LaneResult{
					Key:      lane.Key,
					Hits:     cp,
					Took:     lane.Took,
					Filtered: lane.Filtered,
					Note:     lane.Note,
				})
			}
		}
		if debug.IncludeStages && len(st.Trace) > 0 {
			exec.Stages = make([]retrieval.StageResult, 0, len(st.Trace))
			for _, tr := range st.Trace {
				stage := retrieval.StageResult{
					Name:    tr.Stage,
					Took:    tr.Duration,
					HitsIn:  tr.HitsIn,
					HitsOut: tr.HitsOut,
				}
				if tr.Err != nil {
					stage.Err = tr.Err.Error()
				}
				exec.Stages = append(exec.Stages, stage)
			}
		}
		if debug.IncludeStages && st.HybridExecution != nil {
			exec.Stages = append(exec.Stages, st.HybridExecution.Stages...)
		}
		resp.Execution = exec
		if req.ReturnRaw {
			resp.RawByRetriever = retrieval.ProjectRawByRetriever(exec)
		}
	}
	return resp, nil
}

// currentHitCount returns the most-advanced hit slice length so we can
// approximate hits-in / hits-out per stage without forcing every stage to
// report it explicitly.
func currentHitCount(st *State) int {
	switch {
	case len(st.Final) > 0:
		return len(st.Final)
	case len(st.Reranked) > 0:
		return len(st.Reranked)
	case len(st.Fused) > 0:
		return len(st.Fused)
	default:
		total := 0
		for _, h := range st.Recalls {
			total += len(h)
		}
		return total
	}
}
