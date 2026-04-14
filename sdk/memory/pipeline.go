package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// PipelineState carries data between extraction stages.
type PipelineState struct {
	Input                  ExtractInput
	LLM                    llm.LLM
	Candidates             []CandidateMemory
	DirectSave             []CandidateMemory
	ToDedup                []dedupItem
	Actions                []deduplicationResult
	Saved                  []*MemoryEntry
	DedupFallbackCreateAll bool
}

// Stage is a single step in the extraction pipeline.
type Stage interface {
	Name() string
	Run(ctx context.Context, state *PipelineState) error
}

// Pipeline executes an ordered list of stages.
type Pipeline struct {
	stages []Stage
}

// NewPipeline builds a pipeline from stages.
func NewPipeline(stages ...Stage) *Pipeline {
	return &Pipeline{stages: stages}
}

// Execute runs all stages in order.
func (p *Pipeline) Execute(ctx context.Context, state *PipelineState) error {
	for _, s := range p.stages {
		start := time.Now()
		err := s.Run(ctx, state)
		recordPipelineStage(ctx, s.Name(), float64(time.Since(start).Milliseconds()), err)
		if err != nil {
			return fmt.Errorf("stage %s: %w", s.Name(), err)
		}
	}
	return nil
}
