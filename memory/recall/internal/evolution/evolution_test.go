package evolution

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func scope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

// TestFeedbackBoost_Clamping pins the floor at 0.1 — fusion / rank
// callers rely on a strictly-positive multiplier so heavily-penalised
// facts never multiply scores to zero.
func TestFeedbackBoost_Clamping(t *testing.T) {
	cases := []struct {
		reinf, pen float64
		want       float64
	}{
		{0, 0, 1.0},   // neutral
		{1, 0, 1.05},  // +5%
		{0, 1, 0.95},  // -5%
		{0, 100, 0.1}, // clamp at 0.1
		{2, 1, 1.05},  // (1 + 0.10 - 0.05) = 1.05
	}
	for _, tc := range cases {
		if got := FeedbackBoost(tc.reinf, tc.pen); got != tc.want {
			t.Errorf("FeedbackBoost(%v,%v) = %v, want %v", tc.reinf, tc.pen, got, tc.want)
		}
	}
}

// TestFeedbackBoostFromMeta covers canonical float64 feedback metadata from the
// retrieval lane.
func TestFeedbackBoostFromMeta(t *testing.T) {
	if got := FeedbackBoostFromMeta(nil); got != 1 {
		t.Errorf("nil meta → 1, got %v", got)
	}
	if got := FeedbackBoostFromMeta(map[string]any{}); got != 1 {
		t.Errorf("empty meta → 1, got %v", got)
	}
	if got := FeedbackBoostFromMeta(map[string]any{
		domain.MetaReinforcement: 2.0,
		domain.MetaPenalty:       1.0,
	}); got != FeedbackBoost(2, 1) {
		t.Errorf("float64 meta = %v, want %v", got, FeedbackBoost(2, 1))
	}
}

func TestNopRunners(t *testing.T) {
	ctx := context.Background()
	if err := (NopRunner{}).AfterSave(ctx, scope(), []string{"f1"}); err != nil {
		t.Errorf("NopRunner.AfterSave: %v", err)
	}
	if err := (NopRunner{}).AfterRecall(ctx, scope(), domain.RecallTrace{}); err != nil {
		t.Errorf("NopRunner.AfterRecall: %v", err)
	}
	if err := (NopDecayer{}).Apply(ctx, scope(), time.Now()); err != nil {
		t.Errorf("NopDecayer.Apply: %v", err)
	}
	if err := (NopConsolidator{}).Consolidate(ctx, scope()); err != nil {
		t.Errorf("NopConsolidator.Consolidate: %v", err)
	}
}
