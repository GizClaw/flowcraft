package evolution

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// FeedbackBoost returns the score multiplier derived from fact feedback fields
// for fusion / rank. The boost math stays here because fusion and rank are
// read-side consumers of the canonical reinforcement / penalty fields.
func FeedbackBoost(reinforcement, penalty float64) float64 {
	boost := 1 + reinforcement*0.05 - penalty*0.05
	if boost < 0.1 {
		return 0.1
	}
	return boost
}

// FeedbackBoostFromMeta reads reinforcement / penalty from candidate
// metadata when present (retrieval lane).
func FeedbackBoostFromMeta(meta map[string]any) float64 {
	if meta == nil {
		return 1
	}
	reinf, _ := meta[domain.MetaReinforcement].(float64)
	pen, _ := meta[domain.MetaPenalty].(float64)
	return FeedbackBoost(reinf, pen)
}
