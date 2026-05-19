package temporal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// TestErrNotFound_IsClassifiedAndIdentifiable pins both halves of
// the dual contract spelled out on the sentinel:
//   - public boundary (and HTTP shim) classify it as NotFound
//   - existing errors.Is(err, ErrNotFound) identity checks
//     callers and tests rely on still hold
func TestErrNotFound_IsClassifiedAndIdentifiable(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_, err := s.Get(ctx, scope(), "missing")
	if err == nil {
		t.Fatal("want NotFound")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) lost: %v", err)
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("errdefs.IsNotFound lost classification: %v", err)
	}
}

func TestAppend_DuplicateID_IsConflict(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	f := sampleFact("dup", "k", model.KindNote, time.Unix(1, 0))
	if err := s.Append(ctx, []model.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	err := s.Append(ctx, []model.TemporalFact{f})
	if err == nil {
		t.Fatal("want duplicate id error")
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("duplicate id should map to Conflict: %v", err)
	}
}

func TestAppend_DuplicateIDWithinBatch_IsConflict(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	a := sampleFact("dup", "k1", model.KindNote, time.Unix(1, 0))
	b := sampleFact("dup", "k2", model.KindNote, time.Unix(2, 0))
	err := s.Append(ctx, []model.TemporalFact{a, b})
	if err == nil {
		t.Fatal("want duplicate id error within batch")
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("batch-local duplicate id should map to Conflict: %v", err)
	}
}

func TestAppend_InvalidKind_IsValidation(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	err := s.Append(ctx, []model.TemporalFact{{ID: "x", Scope: scope(), Kind: "bogus"}})
	if err == nil {
		t.Fatal("want invalid kind error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("invalid kind should map to Validation: %v", err)
	}
}

func TestAppend_MissingRuntimeID_IsValidation(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	err := s.Append(ctx, []model.TemporalFact{{ID: "x", Kind: model.KindNote}})
	if err == nil {
		t.Fatal("want missing runtime_id error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id should map to Validation: %v", err)
	}
}

func TestUpdateValidity_ReCloseMismatch_IsConflict(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Append(ctx, []model.TemporalFact{sampleFact("a", "k", model.KindState, time.Unix(1, 0))}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateValidity(ctx, scope(), "a", time.Unix(100, 0), "b"); err != nil {
		t.Fatal(err)
	}
	err := s.UpdateValidity(ctx, scope(), "a", time.Unix(200, 0), "c")
	if err == nil {
		t.Fatal("want re-close mismatch error")
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("re-close mismatch should map to Conflict: %v", err)
	}
}

func TestReopenValidity_GuardMismatch_IsConflict(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Append(ctx, []model.TemporalFact{sampleFact("a", "k", model.KindState, time.Unix(1, 0))}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateValidity(ctx, scope(), "a", time.Unix(100, 0), "b"); err != nil {
		t.Fatal(err)
	}
	err := s.ReopenValidity(ctx, scope(), "a", "not-b")
	if err == nil {
		t.Fatal("want reopen guard mismatch error")
	}
	if !errors.Is(err, ErrReopenConflict) {
		t.Errorf("errors.Is(err, ErrReopenConflict) lost: %v", err)
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("ErrReopenConflict should map to Conflict: %v", err)
	}
}

func TestReopenValidity_Missing_IsNotFound(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	err := s.ReopenValidity(ctx, scope(), "missing", "x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing fact should return ErrNotFound, got %v", err)
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("ErrNotFound from ReopenValidity should map to NotFound: %v", err)
	}
}
