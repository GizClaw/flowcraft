package memory

import (
	"context"
)

// PipelineExtractor orchestrates the synchronous fact extraction pipeline.
type PipelineExtractor struct {
	fact *Pipeline
}

// NewPipelineExtractor builds an extractor around a fact pipeline.
func NewPipelineExtractor(fact *Pipeline) *PipelineExtractor {
	return &PipelineExtractor{fact: fact}
}

// Extract runs the fact pipeline.
func (pe *PipelineExtractor) Extract(ctx context.Context, input ExtractInput) error {
	state := &PipelineState{Input: input}
	return pe.fact.Execute(ctx, state)
}
