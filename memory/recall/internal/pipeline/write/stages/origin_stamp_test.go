package stages_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
)

func TestOriginStamp_StampsResolvedFacts(t *testing.T) {
	s := stages.NewOriginStamp()
	state := &write.WriteState{
		AsyncRequestID: "areq-1",
		SemanticDerivationOrigin: domain.FactOrigin{
			RequestID: "areq-1",
			Kind:      domain.OriginKindSemanticDerivation,
		},
		Resolution: domain.Resolution{
			Facts: []domain.TemporalFact{{ID: "f1"}, {ID: "f2"}},
		},
	}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, f := range state.Resolution.Facts {
		if f.Origin.Kind != domain.OriginKindSemanticDerivation {
			t.Errorf("fact %s origin kind = %q", f.ID, f.Origin.Kind)
		}
	}
}
