package ingest

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// StaticExtractor returns a fixed list of facts on every call. It is
// the test-friendly counterpart to passthroughExtractor for scenarios
// that need deterministic non-empty extraction without involving the
// LLM interface at all.
type StaticExtractor struct {
	Facts []domain.TemporalFact
}

var _ port.Extractor = StaticExtractor{}

// Extract implements port.Extractor.
func (s StaticExtractor) Extract(context.Context, port.IngestInput) ([]domain.TemporalFact, error) {
	out := make([]domain.TemporalFact, len(s.Facts))
	for i, f := range s.Facts {
		out[i] = f.Clone()
	}
	return out, nil
}
