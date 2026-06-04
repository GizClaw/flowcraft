package ingest

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// passthroughExtractor returns caller-supplied Facts verbatim. It is
// the deterministic baseline used when Input.Turns is empty or when
// callers explicitly construct structured facts. Turn-driven
// extraction is opt-in via LLMExtractor.
type passthroughExtractor struct{}

var _ port.Extractor = passthroughExtractor{}

func (passthroughExtractor) CompileExtraction(_ context.Context, input port.IngestInput) (port.ExtractionResult, error) {
	if len(input.Facts) == 0 {
		return port.ExtractionResult{}, nil
	}
	out := make([]domain.TemporalFact, len(input.Facts))
	for i, f := range input.Facts {
		out[i] = f.Clone()
	}
	return port.ExtractionResult{PromotedFacts: out}, nil
}
