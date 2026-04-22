package pipeline

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

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
	Fused         []retrieval.Hit
	Reranked      []retrieval.Hit
	Final         []retrieval.Hit

	ShortCircuit bool

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
func (p *Pipeline) Run(ctx context.Context, idx retrieval.Index, namespace string, req retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	st := &State{
		Index:     idx,
		Namespace: namespace,
		Request:   &req,
		Recalls:   make(map[string][]retrieval.Hit),
	}
	for _, s := range p.stages {
		if st.ShortCircuit {
			break
		}
		t0 := time.Now()
		err := s.Run(ctx, st)
		st.Trace = append(st.Trace, StageTrace{Stage: s.Name(), Duration: time.Since(t0), Err: err})
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
	return &retrieval.SearchResponse{Hits: hits}, nil
}
