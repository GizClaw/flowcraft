package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// --- P1: Multi-scale Search benchmarks ---

func seedLTStore(b *testing.B, n int) *FileLongTermStore {
	b.Helper()
	s := NewFileLongTermStore(workspace.NewMemWorkspace(), "")
	ctx := context.Background()
	categories := AllCategories()
	for i := 0; i < n; i++ {
		cat := categories[i%len(categories)]
		_ = s.Save(ctx, "bench-agent", &MemoryEntry{
			Category: cat,
			Content:  fmt.Sprintf("Memory entry %d about topic %d with details and context", i, i%50),
			Keywords: []string{fmt.Sprintf("topic%d", i%50), "memory"},
		})
	}
	return s
}

func BenchmarkFileLongTermStore_Search(b *testing.B) {
	s := seedLTStore(b, 1000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Search(ctx, "bench-agent", "topic42 memory details", SearchOptions{TopK: 10})
	}
}

func BenchmarkFileLongTermStore_Search_100(b *testing.B) {
	s := seedLTStore(b, 100)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Search(ctx, "bench-agent", "topic42 memory details", SearchOptions{TopK: 10})
	}
}

func BenchmarkFileLongTermStore_Search_10000(b *testing.B) {
	s := seedLTStore(b, 10000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Search(ctx, "bench-agent", "topic42 memory details", SearchOptions{TopK: 10})
	}
}

// --- P0: Tiered injection (MemoryAwareMemory.Load) benchmark ---

// benchLTStore implements LongTermStore with pre-built in-memory data.
type benchLTStore struct {
	data map[MemoryCategory][]*MemoryEntry
}

func (s *benchLTStore) Save(_ context.Context, _ string, _ *MemoryEntry) error { return nil }
func (s *benchLTStore) Update(_ context.Context, _ string, _ *MemoryEntry) error {
	return nil
}
func (s *benchLTStore) Delete(_ context.Context, _, _ string) error { return nil }
func (s *benchLTStore) List(_ context.Context, _ string, opts ListOptions) ([]*MemoryEntry, error) {
	if opts.Category != "" {
		entries := s.data[opts.Category]
		if opts.Limit > 0 && len(entries) > opts.Limit {
			entries = entries[:opts.Limit]
		}
		return entries, nil
	}
	var all []*MemoryEntry
	for _, entries := range s.data {
		all = append(all, entries...)
	}
	if opts.Limit > 0 && len(all) > opts.Limit {
		all = all[:opts.Limit]
	}
	return all, nil
}
func (s *benchLTStore) Search(_ context.Context, _ string, _ string, opts SearchOptions) ([]*MemoryEntry, error) {
	entries := s.data[opts.Category]
	if opts.TopK > 0 && len(entries) > opts.TopK {
		entries = entries[:opts.TopK]
	}
	return entries, nil
}

func newBenchLTStore(profileN, prefN, recallPerCatN int) *benchLTStore {
	s := &benchLTStore{data: make(map[MemoryCategory][]*MemoryEntry)}
	for i := 0; i < profileN; i++ {
		s.data[CategoryProfile] = append(s.data[CategoryProfile], &MemoryEntry{
			ID: fmt.Sprintf("p%d", i), Category: CategoryProfile,
			Content: fmt.Sprintf("Profile fact %d about the user", i),
		})
	}
	for i := 0; i < prefN; i++ {
		s.data[CategoryPreferences] = append(s.data[CategoryPreferences], &MemoryEntry{
			ID: fmt.Sprintf("pref%d", i), Category: CategoryPreferences,
			Content: fmt.Sprintf("Preference %d of the user", i),
		})
	}
	for _, cat := range defaultRecallCategories {
		for i := 0; i < recallPerCatN; i++ {
			s.data[cat] = append(s.data[cat], &MemoryEntry{
				ID: fmt.Sprintf("%s-%d", cat, i), Category: cat,
				Content: fmt.Sprintf("%s entry %d with details", cat, i),
			})
		}
	}
	return s
}

func BenchmarkMemoryAwareLoad_Small(b *testing.B) {
	benchMemoryAwareLoad(b, 3, 2, 5)
}

func BenchmarkMemoryAwareLoad_Medium(b *testing.B) {
	benchMemoryAwareLoad(b, 10, 5, 20)
}

func BenchmarkMemoryAwareLoad_Large(b *testing.B) {
	benchMemoryAwareLoad(b, 20, 10, 50)
}

func benchMemoryAwareLoad(b *testing.B, profileN, prefN, recallPerCatN int) {
	b.Helper()
	msgStore := NewInMemoryStore()
	ctx := context.Background()
	_ = msgStore.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleSystem, "You are helpful."),
		model.NewTextMessage(model.RoleUser, "Tell me about Go concurrency patterns"),
		model.NewTextMessage(model.RoleAssistant, "Go uses goroutines and channels..."),
		model.NewTextMessage(model.RoleUser, "How about error handling?"),
	})

	ltStore := newBenchLTStore(profileN, prefN, recallPerCatN)
	inner := NewBufferMemory(msgStore, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "u1", LongTermConfig{
		Enabled:    true,
		MaxEntries: 20,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = aware.Load(ctx, "c1")
	}
}
