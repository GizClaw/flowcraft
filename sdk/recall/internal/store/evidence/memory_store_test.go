package evidence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

func scope() domain.Scope {
	return domain.Scope{RuntimeID: "rt", UserID: "u1"}
}

func ref(id, msg, text string, ts int64) domain.EvidenceRef {
	return domain.EvidenceRef{
		ID:        id,
		MessageID: msg,
		Role:      "user",
		Text:      text,
		Timestamp: time.Unix(ts, 0),
	}
}

func TestAppend_PersistsAndReturnsInOrder(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Append(ctx, scope(), "f1", []domain.EvidenceRef{
		ref("e1", "m1", "hello", 10),
		ref("e2", "m1", "world", 11),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.ListByFact(ctx, scope(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "e1" || got[1].ID != "e2" {
		t.Errorf("want [e1, e2] in order, got %+v", got)
	}
}

func TestAppend_AssignsStableIDsForEmptyRefs(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	refs := []domain.EvidenceRef{
		{Text: "first"},
		{Text: "second"},
	}
	if err := s.Append(ctx, scope(), "f1", refs); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListByFact(ctx, scope(), "f1")
	if len(got) != 2 || got[0].ID != "f1#0" || got[1].ID != "f1#1" {
		t.Errorf("expected stable auto ids f1#0/f1#1, got %+v", got)
	}
	// replay must not duplicate
	if err := s.Append(ctx, scope(), "f1", refs); err != nil {
		t.Fatal(err)
	}
	again, _ := s.ListByFact(ctx, scope(), "f1")
	if len(again) != 2 {
		t.Errorf("replay should be idempotent, got %d entries", len(again))
	}
}

func TestAppend_IdempotentOnSameIDs(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	refs := []domain.EvidenceRef{ref("e1", "m1", "v1", 1)}
	if err := s.Append(ctx, scope(), "f1", refs); err != nil {
		t.Fatal(err)
	}
	// second append with same id is a no-op on the index, but
	// payload overwrite is allowed (rebuild replays canonical state).
	if err := s.Append(ctx, scope(), "f1", []domain.EvidenceRef{ref("e1", "m1", "v2", 2)}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListByFact(ctx, scope(), "f1")
	if len(got) != 1 || got[0].Text != "v2" {
		t.Errorf("idempotent replay must update payload without duplicating index entries, got %+v", got)
	}
}

func TestAppend_ValidationErrorsClassified(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	err := s.Append(ctx, domain.Scope{}, "f1", []domain.EvidenceRef{ref("e1", "", "", 0)})
	if !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id must be Validation: %v", err)
	}
	err = s.Append(ctx, scope(), "", []domain.EvidenceRef{ref("e1", "", "", 0)})
	if !errdefs.IsValidation(err) {
		t.Errorf("missing fact id must be Validation: %v", err)
	}
}

func TestGet_NotFoundClassified(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_, err := s.Get(ctx, scope(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) lost: %v", err)
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("ErrNotFound should map to NotFound: %v", err)
	}
}

func TestListByFact_EmptyFactIDReturnsEmpty(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Append(ctx, scope(), "f1", []domain.EvidenceRef{ref("e1", "", "", 0)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListByFact(ctx, scope(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty fact id must NOT enumerate scope, got %d entries", len(got))
	}
}

func TestForgetByFact_RemovesIndexAndBlobs(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Append(ctx, scope(), "f1", []domain.EvidenceRef{ref("e1", "", "", 0), ref("e2", "", "", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, scope(), "f2", []domain.EvidenceRef{ref("e3", "", "", 2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetByFact(ctx, scope(), []string{"f1", "missing"}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got, _ := s.ListByFact(ctx, scope(), "f1"); len(got) != 0 {
		t.Errorf("f1 should be empty after forget, got %+v", got)
	}
	if _, err := s.Get(ctx, scope(), "e1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("e1 should be gone, got %v", err)
	}
	if got, _ := s.ListByFact(ctx, scope(), "f2"); len(got) != 1 {
		t.Errorf("ForgetByFact must not touch unrelated facts: %+v", got)
	}
}

func TestStore_IsolatesByScope(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	other := domain.Scope{RuntimeID: "rt", UserID: "u2"}
	if err := s.Append(ctx, scope(), "f1", []domain.EvidenceRef{ref("e1", "", "", 0)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, other, "f1", []domain.EvidenceRef{ref("e1", "", "", 1)}); err != nil {
		t.Fatal(err)
	}
	a, _ := s.ListByFact(ctx, scope(), "f1")
	b, _ := s.ListByFact(ctx, other, "f1")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("scope partitioning broken: a=%+v b=%+v", a, b)
	}
}
