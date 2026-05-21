package evolution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

func scope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

func seedStore(t *testing.T, facts ...domain.TemporalFact) *temporal.MemoryStore {
	t.Helper()
	s := temporal.NewMemoryStore()
	for i := range facts {
		if facts[i].Scope.RuntimeID == "" {
			facts[i].Scope = scope()
		}
		if facts[i].Kind == "" {
			facts[i].Kind = domain.KindNote
		}
		if facts[i].MergeKey == "" {
			facts[i].MergeKey = facts[i].ID + "|k"
		}
		if facts[i].ObservedAt.IsZero() {
			facts[i].ObservedAt = time.Unix(int64(i+1), 0)
		}
	}
	if err := s.Append(context.Background(), facts); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return s
}

// TestReinforceAndPenalize_RoundTripsThroughStore covers the happy
// path for both feedback writers: a positive delta lands on
// Reinforcement, a positive penalty delta lands on Penalty, and
// neither bleed into the other channel.
func TestReinforceAndPenalize_RoundTripsThroughStore(t *testing.T) {
	store := seedStore(t, domain.TemporalFact{ID: "f1"})
	ctx := context.Background()

	if err := Reinforce(ctx, store, scope(), "f1", 2); err != nil {
		t.Fatalf("Reinforce: %v", err)
	}
	if err := Penalize(ctx, store, scope(), "f1", 1); err != nil {
		t.Fatalf("Penalize: %v", err)
	}

	got, err := store.Get(ctx, scope(), "f1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Reinforcement != 2 || got.Penalty != 1 {
		t.Errorf("feedback = (r=%v, p=%v), want (2, 1)", got.Reinforcement, got.Penalty)
	}
}

// TestReinforce_ValidatesInput is the input-contract guard: the
// caller-facing facade leans on these errors to keep callers from
// flushing junk into the feedback channel.
func TestReinforce_ValidatesInput(t *testing.T) {
	ctx := context.Background()
	store := seedStore(t, domain.TemporalFact{ID: "f1"})

	if err := Reinforce(ctx, nil, scope(), "f1", 1); err == nil {
		t.Error("Reinforce(nil store) must error")
	}
	if err := Reinforce(ctx, store, scope(), "", 1); err == nil {
		t.Error("Reinforce(empty factID) must error")
	}
	if err := Reinforce(ctx, store, scope(), "f1", 0); err == nil {
		t.Error("Reinforce(delta=0) must error (must be positive)")
	}
	if err := Reinforce(ctx, store, scope(), "f1", -1); err == nil {
		t.Error("Reinforce(delta<0) must error (must be positive)")
	}
}

func TestPenalize_ValidatesInput(t *testing.T) {
	ctx := context.Background()
	store := seedStore(t, domain.TemporalFact{ID: "f1"})

	if err := Penalize(ctx, nil, scope(), "f1", 1); err == nil {
		t.Error("Penalize(nil store) must error")
	}
	if err := Penalize(ctx, store, scope(), "", 1); err == nil {
		t.Error("Penalize(empty factID) must error")
	}
	if err := Penalize(ctx, store, scope(), "f1", 0); err == nil {
		t.Error("Penalize(delta=0) must error")
	}
}

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

// TestFeedbackBoostFromMeta covers all three numeric metadata
// flavours: native float64 (LLM extractor / JSON), float32 (legacy
// retrieval adapters), and int (hand-crafted callers / tests). All
// must reach the same multiplier as their canonical float64 form.
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
	if got := FeedbackBoostFromMeta(map[string]any{
		domain.MetaReinforcement: float32(2),
		domain.MetaPenalty:       float32(1),
	}); got != FeedbackBoost(2, 1) {
		t.Errorf("float32 meta = %v, want %v", got, FeedbackBoost(2, 1))
	}
	if got := FeedbackBoostFromMeta(map[string]any{
		domain.MetaReinforcement: 2,
		domain.MetaPenalty:       1,
	}); got != FeedbackBoost(2, 1) {
		t.Errorf("int meta = %v, want %v", got, FeedbackBoost(2, 1))
	}
}

// TestExpireRetired_SweepsByExpiresAt pins the D.8 sweep contract:
// only facts whose ExpiresAt is non-zero AND not-after now are
// removed; everything else (open-ended / future / zero-time) stays.
func TestExpireRetired_SweepsByExpiresAt(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	zero := time.Time{}

	store := seedStore(t,
		domain.TemporalFact{ID: "expired", ExpiresAt: &past},
		domain.TemporalFact{ID: "future", ExpiresAt: &future},
		domain.TemporalFact{ID: "zero", ExpiresAt: &zero},
		domain.TemporalFact{ID: "open"},
	)
	ctx := context.Background()

	expired, err := ExpireRetired(ctx, store, scope(), now)
	if err != nil {
		t.Fatalf("ExpireRetired: %v", err)
	}
	if len(expired) != 1 || expired[0] != "expired" {
		t.Errorf("expired = %v, want [expired]", expired)
	}
	if _, err := store.Get(ctx, scope(), "expired"); err == nil {
		t.Error("expected expired fact to be deleted from store")
	}
	for _, id := range []string{"future", "zero", "open"} {
		if _, err := store.Get(ctx, scope(), id); err != nil {
			t.Errorf("fact %q must survive sweep, got err %v", id, err)
		}
	}
}

func TestExpireRetired_NoExpiredReturnsNil(t *testing.T) {
	store := seedStore(t, domain.TemporalFact{ID: "open"})
	got, err := ExpireRetired(context.Background(), store, scope(), time.Now())
	if err != nil {
		t.Fatalf("ExpireRetired: %v", err)
	}
	if got != nil {
		t.Errorf("no expired facts → nil slice, got %v", got)
	}
}

// errStore returns a failure for List so ExpireRetired's
// list-side error path stays covered.
type errStore struct {
	port.TemporalStore
	listErr error
}

func (s errStore) List(context.Context, domain.Scope, port.ListQuery) ([]domain.TemporalFact, error) {
	return nil, s.listErr
}

func TestExpireRetired_ListErrorPropagates(t *testing.T) {
	boom := errors.New("list failed")
	_, err := ExpireRetired(context.Background(), errStore{listErr: boom}, scope(), time.Now())
	if !errors.Is(err, boom) {
		t.Errorf("List error must propagate, got %v", err)
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
