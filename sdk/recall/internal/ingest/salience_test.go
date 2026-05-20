package ingest

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

func TestSalience_TierMapping(t *testing.T) {
	tests := []struct {
		tier string
		want float64
	}{
		{domain.TierCore, 0.8},
		{domain.TierGeneral, 0.5},
		{domain.TierData, 0.4},
		{domain.TierStorage, 0.2},
	}
	for _, tc := range tests {
		got := defaultSalienceScorer{tier: tc.tier}.Score(domain.TemporalFact{}).Confidence
		if got != tc.want {
			t.Errorf("tier %q confidence = %v want %v", tc.tier, got, tc.want)
		}
	}
}
