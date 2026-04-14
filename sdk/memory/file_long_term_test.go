package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func newTestStore(opts ...LTStoreOption) *FileLongTermStore {
	return NewFileLongTermStore(workspace.NewMemWorkspace(), "", opts...)
}

func TestFileLongTermStore_Update_EmptyCategory(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	entry := &MemoryEntry{
		Category: CategoryProfile,
		Content:  "User is a Go developer",
		Keywords: []string{"go", "developer"},
		Source:   MemorySource{RuntimeID: "owner"},
	}
	if err := s.Save(ctx, "agent1", entry); err != nil {
		t.Fatal(err)
	}

	// Update without specifying category
	updated := &MemoryEntry{
		ID:      entry.ID,
		Content: "User is a senior Go developer",
	}
	if err := s.Update(ctx, "agent1", updated); err != nil {
		t.Fatal(err)
	}

	entries, _ := s.List(ctx, "agent1", ListOptions{Category: CategoryProfile})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Content != "User is a senior Go developer" {
		t.Fatalf("content not updated: %q", entries[0].Content)
	}
	// Preserved fields
	if len(entries[0].Keywords) == 0 {
		t.Fatal("keywords should be preserved")
	}
	if entries[0].Source.RuntimeID != "owner" {
		t.Fatal("source should be preserved")
	}
	if entries[0].CreatedAt.IsZero() {
		t.Fatal("created_at should be preserved")
	}
}

func TestFileLongTermStore_Update_WithCategory(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	entry := &MemoryEntry{
		Category: CategoryEntities,
		Content:  "Project Alpha",
		Keywords: []string{"alpha"},
	}
	if err := s.Save(ctx, "agent1", entry); err != nil {
		t.Fatal(err)
	}

	updated := &MemoryEntry{
		ID:       entry.ID,
		Category: CategoryEntities,
		Content:  "Project Alpha v2",
	}
	if err := s.Update(ctx, "agent1", updated); err != nil {
		t.Fatal(err)
	}

	entries, _ := s.List(ctx, "agent1", ListOptions{Category: CategoryEntities})
	if len(entries) != 1 {
		t.Fatalf("expected 1, got %d", len(entries))
	}
	if entries[0].Content != "Project Alpha v2" {
		t.Fatalf("unexpected content: %q", entries[0].Content)
	}
}

func TestFileLongTermStore_MaxEntries_Eviction(t *testing.T) {
	s := newTestStore(WithMaxEntries(3))
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		entry := &MemoryEntry{
			Category:  CategoryEvents,
			Content:   "event " + string(rune('A'+i)),
			UpdatedAt: time.Now().Add(time.Duration(i) * time.Minute),
		}
		if err := s.Save(ctx, "agent1", entry); err != nil {
			t.Fatal(err)
		}
	}

	entries, _ := s.List(ctx, "agent1", ListOptions{Category: CategoryEvents})
	if len(entries) != 3 {
		t.Fatalf("expected 3 after eviction, got %d", len(entries))
	}

	// Should keep the most recent entries (C, D, E)
	for _, e := range entries {
		if e.Content == "event A" || e.Content == "event B" {
			t.Fatalf("oldest entries should be evicted, found %q", e.Content)
		}
	}
}

func TestFileLongTermStore_Cache_HitAfterSave(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	entry := &MemoryEntry{
		Category: CategoryProfile,
		Content:  "cached entry",
	}
	_ = s.Save(ctx, "agent1", entry)

	// Search should work from cache without additional disk read
	results, err := s.Search(ctx, "agent1", "cached entry", SearchOptions{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search to find cached entry")
	}
}

func TestFileLongTermStore_Search_CJK(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	_ = s.Save(ctx, "agent1", &MemoryEntry{
		Category: CategoryCases,
		Content:  "Go 并发编程最佳实践",
		Keywords: []string{"go", "并发", "编程"},
	})
	_ = s.Save(ctx, "agent1", &MemoryEntry{
		Category: CategoryCases,
		Content:  "React hooks tutorial",
		Keywords: []string{"react", "hooks"},
	})

	results, _ := s.Search(ctx, "agent1", "并发", SearchOptions{
		Category: CategoryCases,
		TopK:     5,
	})
	if len(results) == 0 {
		t.Fatal("expected CJK search to find matching entry")
	}
	if !strings.Contains(results[0].Content, "并发") {
		t.Fatalf("unexpected result: %q", results[0].Content)
	}
}

func TestFileLongTermStore_CorpusIsolation(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	// Save entries in different categories
	_ = s.Save(ctx, "agent1", &MemoryEntry{
		Category: CategoryProfile,
		Content:  "Developer profile info",
	})
	_ = s.Save(ctx, "agent1", &MemoryEntry{
		Category: CategoryEvents,
		Content:  "Important meeting notes about development",
	})

	// Corpus stats should be isolated per category
	s.mu.RLock()
	profileCorpus := s.corpora["agent1"][CategoryProfile]
	eventsCorpus := s.corpora["agent1"][CategoryEvents]
	s.mu.RUnlock()

	if profileCorpus == nil || eventsCorpus == nil {
		t.Fatal("corpora should exist for both categories")
	}
	if profileCorpus.DocCount != 1 || eventsCorpus.DocCount != 1 {
		t.Fatalf("each corpus should have 1 doc, got profile=%d events=%d",
			profileCorpus.DocCount, eventsCorpus.DocCount)
	}
}

func TestFileLongTermStore_NoMaxEntries(t *testing.T) {
	s := newTestStore() // no WithMaxEntries, defaults to 0 (unlimited)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		_ = s.Save(ctx, "agent1", &MemoryEntry{
			Category: CategoryProfile,
			Content:  "entry",
		})
	}

	entries, _ := s.List(ctx, "agent1", ListOptions{Category: CategoryProfile})
	if len(entries) != 10 {
		t.Fatalf("no eviction expected, got %d", len(entries))
	}
}

func TestFileLongTermStore_LargeEntries(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	bigContent := strings.Repeat("large content ", 5000) // ~70KB
	entry := &MemoryEntry{
		Category: CategoryCases,
		Content:  bigContent,
		Keywords: []string{"large"},
	}
	if err := s.Save(ctx, "agent1", entry); err != nil {
		t.Fatal(err)
	}

	entries, err := s.List(ctx, "agent1", ListOptions{Category: CategoryCases})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1, got %d", len(entries))
	}
	if len(entries[0].Content) != len(bigContent) {
		t.Fatalf("content length mismatch: expected %d, got %d", len(bigContent), len(entries[0].Content))
	}
}

func TestFileLongTermStore_Delete_CacheIntegrity(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	// Save 3 entries
	ids := make([]string, 3)
	for i := 0; i < 3; i++ {
		entry := &MemoryEntry{
			Category: CategoryProfile,
			Content:  fmt.Sprintf("entry-%d", i),
		}
		if err := s.Save(ctx, "agent1", entry); err != nil {
			t.Fatal(err)
		}
		ids[i] = entry.ID
	}

	// Snapshot entries before delete
	before, _ := s.List(ctx, "agent1", ListOptions{Category: CategoryProfile})
	if len(before) != 3 {
		t.Fatalf("expected 3, got %d", len(before))
	}

	// Delete middle entry
	if err := s.Delete(ctx, "agent1", ids[1]); err != nil {
		t.Fatal(err)
	}

	// The "before" slice should not have been corrupted
	// (C3 fix ensures a new slice is created for the write)
	after, _ := s.List(ctx, "agent1", ListOptions{Category: CategoryProfile})
	if len(after) != 2 {
		t.Fatalf("expected 2 after delete, got %d", len(after))
	}
	for _, e := range after {
		if e.ID == ids[1] {
			t.Fatal("deleted entry should not appear")
		}
	}
}

func TestFileLongTermStore_Update_NoCallerMutation(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	original := &MemoryEntry{
		Category: CategoryProfile,
		Content:  "original content",
		Keywords: []string{"key1"},
		Source:   MemorySource{RuntimeID: "owner"},
	}
	if err := s.Save(ctx, "agent1", original); err != nil {
		t.Fatal(err)
	}

	// Create an update entry - only specify ID and new Content
	updateReq := &MemoryEntry{
		ID:      original.ID,
		Content: "updated content",
	}
	if err := s.Update(ctx, "agent1", updateReq); err != nil {
		t.Fatal(err)
	}

	// The updateReq should NOT have been mutated with the old entry's fields
	if updateReq.Category != "" {
		t.Fatalf("caller's entry was mutated: Category=%q", updateReq.Category)
	}
	if len(updateReq.Keywords) != 0 {
		t.Fatalf("caller's entry was mutated: Keywords=%v", updateReq.Keywords)
	}

	// But the stored entry should have the merged fields
	entries, _ := s.List(ctx, "agent1", ListOptions{Category: CategoryProfile})
	if len(entries) != 1 {
		t.Fatalf("expected 1, got %d", len(entries))
	}
	if entries[0].Content != "updated content" {
		t.Fatal("content should be updated")
	}
	if len(entries[0].Keywords) == 0 || entries[0].Keywords[0] != "key1" {
		t.Fatal("keywords should be preserved from original")
	}
}

func TestFileLongTermStore_Search_TimeDecay(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	now := time.Now()

	// Recent entry: updated 1 day ago
	recent := &MemoryEntry{
		Category:  CategoryEvents,
		Content:   "deployed service alpha",
		Keywords:  []string{"deploy", "alpha"},
		UpdatedAt: now.Add(-24 * time.Hour),
	}
	if err := s.Save(ctx, "agent1", recent); err != nil {
		t.Fatal(err)
	}
	// Force UpdatedAt (Save overwrites it)
	s.mu.Lock()
	s.cache["agent1"][CategoryEvents][0].UpdatedAt = now.Add(-24 * time.Hour)
	s.mu.Unlock()

	// Old entry: updated 60 days ago (same keywords, should rank lower)
	old := &MemoryEntry{
		Category:  CategoryEvents,
		Content:   "deployed service alpha legacy",
		Keywords:  []string{"deploy", "alpha"},
		UpdatedAt: now.Add(-60 * 24 * time.Hour),
	}
	if err := s.Save(ctx, "agent1", old); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cache["agent1"][CategoryEvents][1].UpdatedAt = now.Add(-60 * 24 * time.Hour)
	s.mu.Unlock()

	results, err := s.Search(ctx, "agent1", "deployed alpha", SearchOptions{
		Category: CategoryEvents,
		TopK:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// The recent entry should rank higher due to time decay
	if results[0].ID != recent.ID {
		t.Fatalf("recent entry should rank first, got %q first", results[0].Content)
	}
}

func TestTimeDecay_Exported(t *testing.T) {
	now := time.Now()

	// Zero days ago → decay ≈ 1.0
	d := TimeDecay(now, CategoryEvents, now)
	if d < 0.99 || d > 1.01 {
		t.Fatalf("expected ~1.0 for now, got %f", d)
	}

	// 30 days ago for events (half-life=30) → decay ≈ 0.5
	d = TimeDecay(now.Add(-30*24*time.Hour), CategoryEvents, now)
	if d < 0.49 || d > 0.51 {
		t.Fatalf("expected ~0.5 for events at half-life, got %f", d)
	}

	// 365 days ago for profile (half-life=365) → decay ≈ 0.5
	d = TimeDecay(now.Add(-365*24*time.Hour), CategoryProfile, now)
	if d < 0.49 || d > 0.51 {
		t.Fatalf("expected ~0.5 for profile at half-life, got %f", d)
	}

	// 30 days ago for profile (half-life=365) → should still be high (~0.94)
	d = TimeDecay(now.Add(-30*24*time.Hour), CategoryProfile, now)
	if d < 0.93 {
		t.Fatalf("expected high decay for profile at 30 days, got %f", d)
	}

	// Future timestamp should clamp to 1.0
	d = TimeDecay(now.Add(24*time.Hour), CategoryEvents, now)
	if d < 0.99 || d > 1.01 {
		t.Fatalf("expected ~1.0 for future timestamp, got %f", d)
	}
}

func TestFileLongTermStore_CustomCategory_SaveAndSearch(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	customCat := MemoryCategory("people")
	_ = s.Save(ctx, "agent1", &MemoryEntry{
		Category: customCat,
		Content:  "User has a sister named Alice",
		Keywords: []string{"sister", "Alice"},
	})
	_ = s.Save(ctx, "agent1", &MemoryEntry{
		Category: CategoryProfile,
		Content:  "User is 25 years old",
		Keywords: []string{"age"},
	})

	// List with custom category
	entries, err := s.List(ctx, "agent1", ListOptions{Category: customCat})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for custom category, got %d", len(entries))
	}
	if entries[0].Content != "User has a sister named Alice" {
		t.Fatalf("unexpected content: %q", entries[0].Content)
	}

	// List all categories should include custom category
	all, err := s.List(ctx, "agent1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 total entries, got %d", len(all))
	}

	// Search without category should find custom category entries
	results, err := s.Search(ctx, "agent1", "sister Alice", SearchOptions{TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("search should find custom category entry")
	}
	found := false
	for _, r := range results {
		if r.Category == customCat {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("search results should include custom category entry")
	}
}

func TestFileLongTermStore_CustomCategory_UpdateAndDelete(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	customCat := MemoryCategory("emotions")
	entry := &MemoryEntry{
		Category: customCat,
		Content:  "User felt happy today",
		Keywords: []string{"happy"},
	}
	_ = s.Save(ctx, "agent1", entry)

	// Update
	err := s.Update(ctx, "agent1", &MemoryEntry{
		ID:       entry.ID,
		Category: customCat,
		Content:  "User felt very happy today",
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, _ := s.List(ctx, "agent1", ListOptions{Category: customCat})
	if len(entries) != 1 || entries[0].Content != "User felt very happy today" {
		t.Fatal("update should work for custom category")
	}

	// Delete
	err = s.Delete(ctx, "agent1", entry.ID)
	if err != nil {
		t.Fatal(err)
	}

	entries, _ = s.List(ctx, "agent1", ListOptions{Category: customCat})
	if len(entries) != 0 {
		t.Fatal("delete should work for custom category")
	}
}

func TestFileLongTermStore_ConcurrentSearchNoDeadlock(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		_ = s.Save(ctx, "agent1", &MemoryEntry{
			Category: CategoryProfile,
			Content:  fmt.Sprintf("entry %d about golang and concurrency", i),
			Keywords: []string{"go", "concurrency"},
		})
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Search(ctx, "agent1", "golang concurrency", SearchOptions{TopK: 5})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.List(ctx, "agent1", ListOptions{Category: CategoryProfile, Limit: 10})
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Search/List deadlocked")
	}
}

func TestCategoryHalfLife(t *testing.T) {
	cases := []struct {
		cat      MemoryCategory
		expected float64
	}{
		{CategoryProfile, 365},
		{CategoryPreferences, 365},
		{CategoryEntities, 90},
		{CategoryEvents, 30},
		{CategoryCases, 60},
		{CategoryPatterns, 180},
		{MemoryCategory("unknown"), 90},
	}
	for _, tc := range cases {
		got := CategoryHalfLife(tc.cat)
		if got != tc.expected {
			t.Errorf("CategoryHalfLife(%q) = %f, want %f", tc.cat, got, tc.expected)
		}
	}
}
