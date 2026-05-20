package evolution

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Reinforce applies a positive feedback delta to a canonical fact.
func Reinforce(ctx context.Context, store port.TemporalStore, scope domain.Scope, factID string, delta float64) error {
	if store == nil {
		return fmt.Errorf("recall reinforce: store is required")
	}
	if factID == "" {
		return fmt.Errorf("recall reinforce: fact id is required")
	}
	if delta <= 0 {
		return fmt.Errorf("recall reinforce: delta must be positive")
	}
	if _, err := store.Get(ctx, scope, factID); err != nil {
		if err := mapStoreErr(err); err != nil {
			return err
		}
	}
	return store.UpdateFeedback(ctx, scope, factID, delta, 0)
}

// Penalize applies a negative feedback delta to a canonical fact.
func Penalize(ctx context.Context, store port.TemporalStore, scope domain.Scope, factID string, delta float64) error {
	if store == nil {
		return fmt.Errorf("recall penalize: store is required")
	}
	if factID == "" {
		return fmt.Errorf("recall penalize: fact id is required")
	}
	if delta <= 0 {
		return fmt.Errorf("recall penalize: delta must be positive")
	}
	if _, err := store.Get(ctx, scope, factID); err != nil {
		if err := mapStoreErr(err); err != nil {
			return err
		}
	}
	return store.UpdateFeedback(ctx, scope, factID, 0, delta)
}

func mapStoreErr(err error) error {
	if err == nil {
		return nil
	}
	// Preserve store classification for public boundary mapping.
	return err
}

// FeedbackBoost returns the score multiplier derived from fact
// feedback fields for fusion / rank (Phase D.4).
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
	if reinf == 0 && pen == 0 {
		switch v := meta[domain.MetaReinforcement].(type) {
		case float32:
			reinf = float64(v)
		case int:
			reinf = float64(v)
		}
		switch v := meta[domain.MetaPenalty].(type) {
		case float32:
			pen = float64(v)
		case int:
			pen = float64(v)
		}
	}
	return FeedbackBoost(reinf, pen)
}
