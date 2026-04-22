package recall

import (
	"context"
	"testing"
	"time"

	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func newRLTM(t *testing.T) *RetrievalStore {
	t.Helper()
	return NewRetrievalStore(memidx.New())
}

func TestRetrievalLongTerm_SaveAndSearch(t *testing.T) {
	ctx := context.Background()
	s := newRLTM(t)
	if err := s.Save(ctx, "rt1", &MemoryEntry{
		Category: CategoryProfile,
		Content:  "Alice is a Go backend developer at Acme.",
		Scope:    MemoryScope{RuntimeID: "rt1", UserID: "u1"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	res, err := s.Search(ctx, "rt1", "Alice", SearchOptions{
		Category: CategoryProfile,
		TopK:     5,
		Scope:    &MemoryScope{RuntimeID: "rt1", UserID: "u1"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("expected hits")
	}
	if res[0].Scope.UserID != "u1" {
		t.Fatalf("unexpected scope %+v", res[0].Scope)
	}
}

func TestRetrievalLongTerm_ListAndDelete(t *testing.T) {
	ctx := context.Background()
	s := newRLTM(t)
	for i, c := range []string{"one", "two", "three"} {
		if err := s.Save(ctx, "rt1", &MemoryEntry{
			Category: CategoryEvents,
			Content:  c,
			Scope:    MemoryScope{RuntimeID: "rt1", UserID: "u1"},
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	scope := &MemoryScope{RuntimeID: "rt1", UserID: "u1"}
	entries, err := s.List(ctx, "rt1", ListOptions{Category: CategoryEvents, Limit: 10, Scope: scope})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3, got %d", len(entries))
	}
	if err := s.DeleteScoped(ctx, "rt1", *scope, entries[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	entries, _ = s.List(ctx, "rt1", ListOptions{Category: CategoryEvents, Limit: 10, Scope: scope})
	if len(entries) != 2 {
		t.Fatalf("after delete want 2, got %d", len(entries))
	}
}

func TestRetrievalLongTerm_ScopeFilter(t *testing.T) {
	ctx := context.Background()
	s := newRLTM(t)
	_ = s.Save(ctx, "rt1", &MemoryEntry{
		Category: CategoryProfile,
		Content:  "Alice loves Go.",
		Scope:    MemoryScope{RuntimeID: "rt1", UserID: "u1"},
	})
	_ = s.Save(ctx, "rt1", &MemoryEntry{
		Category: CategoryProfile,
		Content:  "Bob loves Python.",
		Scope:    MemoryScope{RuntimeID: "rt1", UserID: "u2"},
	})
	res, err := s.Search(ctx, "rt1", "loves", SearchOptions{
		Category: CategoryProfile,
		TopK:     10,
		Scope:    &MemoryScope{RuntimeID: "rt1", UserID: "u1"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].Scope.UserID != "u1" {
		t.Fatalf("scope filter failed: %+v", res)
	}
}

func TestRetrievalLongTerm_RoundtripFields(t *testing.T) {
	ctx := context.Background()
	s := newRLTM(t)
	exp := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	in := &MemoryEntry{
		Category:   CategorySemantic,
		Content:    "fact",
		Categories: []string{"semantic", "episodic"},
		Entities:   []string{"alice", "go"},
		Confidence: 0.85,
		ExpiresAt:  &exp,
		Scope:      MemoryScope{RuntimeID: "rt1", UserID: "u1", AgentID: "agent-x"},
	}
	if err := s.Save(ctx, "rt1", in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := s.List(ctx, "rt1", ListOptions{
		Category: CategorySemantic, Limit: 1,
		Scope: &MemoryScope{RuntimeID: "rt1", UserID: "u1"},
	})
	if err != nil || len(out) != 1 {
		t.Fatalf("list: %v %v", err, out)
	}
	got := out[0]
	if got.Confidence != 0.85 {
		t.Fatalf("confidence: %v", got.Confidence)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at: %v vs %v", got.ExpiresAt, exp)
	}
	if got.Scope.AgentID != "agent-x" {
		t.Fatalf("agent_id lost: %+v", got.Scope)
	}
	if len(got.Entities) != 2 {
		t.Fatalf("entities: %v", got.Entities)
	}
}
