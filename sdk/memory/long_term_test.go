package memory

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestFileLongTermStore_Search_English(t *testing.T) {
	s := NewFileLongTermStore(workspace.NewMemWorkspace(), "")
	ctx := context.Background()

	_ = s.Save(ctx, "a1", &MemoryEntry{
		Category: CategoryProfile,
		Content:  "Go is a great programming language",
		Keywords: []string{"go", "programming"},
	})

	results, err := s.Search(ctx, "a1", "programming language", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for matching query")
	}
}

func TestFileLongTermStore_Search_NoMatch(t *testing.T) {
	s := NewFileLongTermStore(workspace.NewMemWorkspace(), "")
	ctx := context.Background()

	_ = s.Save(ctx, "a1", &MemoryEntry{
		Category: CategoryProfile,
		Content:  "Python is awesome",
		Keywords: []string{"python"},
	})

	// Use positive threshold so zero-score (non-matching) docs are excluded
	results, err := s.Search(ctx, "a1", "golang concurrency", SearchOptions{TopK: 5, Threshold: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results for non-matching query, got %d", len(results))
	}
}

func TestFileLongTermStore_Search_CJKIntegration(t *testing.T) {
	s := NewFileLongTermStore(workspace.NewMemWorkspace(), "")
	ctx := context.Background()

	_ = s.Save(ctx, "a1", &MemoryEntry{
		Category: CategoryCases,
		Content:  "Go 并发编程最佳实践",
		Keywords: []string{"并发", "go"},
	})

	results, err := s.Search(ctx, "a1", "并发", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected CJK search to find result")
	}
}

func TestFileLongTermStore_Search_EmptyQuery(t *testing.T) {
	s := NewFileLongTermStore(workspace.NewMemWorkspace(), "")
	ctx := context.Background()

	_ = s.Save(ctx, "a1", &MemoryEntry{
		Category: CategoryProfile,
		Content:  "some content",
	})

	results, err := s.Search(ctx, "a1", "", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatal("empty query should return no results")
	}
}

func TestEntryMatchesQueryScope(t *testing.T) {
	t.Parallel()
	u1 := MemoryScope{UserID: "alice"}
	u2 := MemoryScope{UserID: "bob"}
	eAlice := &MemoryEntry{Scope: MemoryScope{UserID: "alice", SessionID: "t1"}}
	eBob := &MemoryEntry{Scope: MemoryScope{UserID: "bob", SessionID: "t1"}}

	if !EntryMatchesQueryScope(eAlice, nil) {
		t.Fatal("nil query should match")
	}
	if EntryMatchesQueryScope(eAlice, &u2) {
		t.Fatal("different user should not match")
	}
	if !EntryMatchesQueryScope(eAlice, &u1) {
		t.Fatal("same user without session filter should match")
	}
	if EntryMatchesQueryScope(eBob, &u1) {
		t.Fatal("bob entry should not match alice query")
	}

	qThread := MemoryScope{UserID: "alice", SessionID: "t1"}
	if !EntryMatchesQueryScope(eAlice, &qThread) {
		t.Fatal("exact session should match")
	}
	eAliceUnscoped := &MemoryEntry{Scope: MemoryScope{UserID: "alice", SessionID: ""}}
	if !EntryMatchesQueryScope(eAliceUnscoped, &qThread) {
		t.Fatal("user-scoped row with empty session matches thread query")
	}
	eOtherThread := &MemoryEntry{Scope: MemoryScope{UserID: "alice", SessionID: "t2"}}
	if EntryMatchesQueryScope(eOtherThread, &qThread) {
		t.Fatal("different session should not match when query pins session")
	}

	gq := MemoryScope{} // IsGlobal() — no UserID
	eg := &MemoryEntry{Scope: MemoryScope{}}
	if !EntryMatchesQueryScope(eg, &gq) {
		t.Fatal("global row should match global query")
	}
	if EntryMatchesQueryScope(eAlice, &gq) {
		t.Fatal("scoped row should not match global query")
	}
}
