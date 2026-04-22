package recall

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// scopedMemoryLTStore is an in-memory LongTermStore that filters List/Search with
// EntryMatchesQueryScope (same contract as FileLongTermStore / Postgres).
type scopedMemoryLTStore struct {
	mu    sync.Mutex
	byRun map[string]map[MemoryCategory][]*MemoryEntry
}

func newScopedMemoryLTStore() *scopedMemoryLTStore {
	return &scopedMemoryLTStore{byRun: make(map[string]map[MemoryCategory][]*MemoryEntry)}
}

func (s *scopedMemoryLTStore) seed(runtimeID string, entries ...*MemoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byRun[runtimeID] == nil {
		s.byRun[runtimeID] = make(map[MemoryCategory][]*MemoryEntry)
	}
	for _, e := range entries {
		c := e.Category
		s.byRun[runtimeID][c] = append(s.byRun[runtimeID][c], e)
	}
}

func (s *scopedMemoryLTStore) List(_ context.Context, runtimeID string, opts ListOptions) ([]*MemoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.byRun[runtimeID][opts.Category]
	rec := EffectiveRecallForList(opts, runtimeID)
	var out []*MemoryEntry
	for _, e := range src {
		if rec != nil {
			if !EntryMatchesRecallScope(e, rec) {
				continue
			}
		} else if !EntryMatchesQueryScope(e, opts.Scope) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func scopeTestQueryMatchesDoc(docLower, queryLower string) bool {
	if queryLower == "" {
		return true
	}
	if strings.Contains(docLower, queryLower) {
		return true
	}
	for _, w := range strings.Fields(queryLower) {
		if len(w) < 3 {
			continue
		}
		if strings.Contains(docLower, w) {
			return true
		}
	}
	return false
}

func (s *scopedMemoryLTStore) Search(_ context.Context, runtimeID, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.byRun[runtimeID][opts.Category]
	q := strings.ToLower(strings.TrimSpace(query))
	rec := EffectiveRecallForSearch(opts, runtimeID)
	var out []*MemoryEntry
	for _, e := range src {
		if rec != nil {
			if !EntryMatchesRecallScope(e, rec) {
				continue
			}
		} else if !EntryMatchesQueryScope(e, opts.Scope) {
			continue
		}
		doc := strings.ToLower(e.Content + " " + strings.Join(e.Keywords, " "))
		if scopeTestQueryMatchesDoc(doc, q) {
			out = append(out, e)
		}
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 10
	}
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func (s *scopedMemoryLTStore) Save(context.Context, string, *MemoryEntry) error   { return nil }
func (s *scopedMemoryLTStore) Update(context.Context, string, *MemoryEntry) error { return nil }
func (s *scopedMemoryLTStore) Delete(context.Context, string, string) error       { return nil }

func TestAssembler_ScopeEnabled_GlobalPinned_UserScopedRecall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewInMemoryStore()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		// Avoid shared token "project" (both rows contain it) — use alpha / Go.
		model.NewTextMessage(model.RoleUser, "tell me about alpha and Go services"),
	})
	inner := NewBufferMemory(store, 50)

	lt := newScopedMemoryLTStore()
	rt := "runtime-scope-1"
	lt.seed(rt,
		&MemoryEntry{ID: "pg", Category: CategoryProfile, Content: "Shared tapdoki tagline: fox mascot", Scope: MemoryScope{}},
		&MemoryEntry{ID: "pu-alice", Category: CategoryProfile, Content: "Private Alice profile row should not pin globally", Scope: MemoryScope{UserID: "alice"}},
		&MemoryEntry{ID: "ent-alice", Category: CategoryEntities, Content: "Alice project alpha uses Go", Scope: MemoryScope{UserID: "alice"}, Keywords: []string{"alpha"}},
		&MemoryEntry{ID: "ent-bob", Category: CategoryEntities, Content: "Bob project beta uses Rust", Scope: MemoryScope{UserID: "bob"}, Keywords: []string{"beta"}},
	)

	aware := NewMemoryAwareMemoryCompat(inner, lt, rt, LongTermConfig{
		Enabled:          true,
		ScopeEnabled:     true,
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})
	aware.SetScope(&MemoryScope{UserID: "alice"})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	sys := msgs[0].Content()

	if !strings.Contains(sys, "fox mascot") {
		t.Fatalf("pinned must load global profile: %q", sys)
	}
	if strings.Contains(sys, "Private Alice profile row") {
		t.Fatalf("user-scoped profile must not be listed under global pinned scope: %q", sys)
	}
	if !strings.Contains(sys, "project alpha") || !strings.Contains(sys, "Go") {
		t.Fatalf("alice-scoped recall should include her entity: %q", sys)
	}
	if strings.Contains(sys, "Rust") || strings.Contains(sys, "beta") {
		t.Fatalf("bob entity must not leak into alice recall: %q", sys)
	}
}

func TestAssembler_ScopeEnabled_OtherUserRecallIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewInMemoryStore()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "how is beta and Rust performance"),
	})
	inner := NewBufferMemory(store, 50)

	lt := newScopedMemoryLTStore()
	rt := "runtime-scope-2"
	lt.seed(rt,
		&MemoryEntry{ID: "pg", Category: CategoryProfile, Content: "Shared tapdoki tagline: fox mascot", Scope: MemoryScope{}},
		&MemoryEntry{ID: "ent-alice", Category: CategoryEntities, Content: "Alice project alpha uses Go", Scope: MemoryScope{UserID: "alice"}},
		&MemoryEntry{ID: "ent-bob", Category: CategoryEntities, Content: "Bob project beta uses Rust", Scope: MemoryScope{UserID: "bob"}},
	)

	aware := NewMemoryAwareMemoryCompat(inner, lt, rt, LongTermConfig{
		Enabled:          true,
		ScopeEnabled:     true,
		MaxEntries:       20,
		PinnedCategories: []MemoryCategory{CategoryProfile},
		RecallCategories: []MemoryCategory{CategoryEntities},
	})
	aware.SetScope(&MemoryScope{UserID: "bob"})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	sys := msgs[0].Content()

	if !strings.Contains(sys, "fox mascot") {
		t.Fatalf("bob still gets shared pinned: %q", sys)
	}
	if !strings.Contains(sys, "beta") || !strings.Contains(sys, "Rust") {
		t.Fatalf("bob recall: %q", sys)
	}
	if strings.Contains(sys, "Alice project alpha") {
		t.Fatalf("alice entity leaked into bob recall: %q", sys)
	}
}

func TestAssembler_RecallPartitions_UserUnionGlobal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewInMemoryStore()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "tell me about February expeditions caves and secret lake"),
	})
	inner := NewBufferMemory(store, 50)

	adv := MemoryCategory("adventure")
	lt := newScopedMemoryLTStore()
	rt := "runtime-adv-1"
	lt.seed(rt,
		&MemoryEntry{ID: "adv-global", Category: adv, Content: "February cave expedition: found glowing mushrooms", Scope: MemoryScope{}},
		&MemoryEntry{ID: "adv-alice", Category: adv, Content: "February Alice-only detour: secret lake campsite", Scope: MemoryScope{UserID: "alice"}},
		&MemoryEntry{ID: "adv-bob", Category: adv, Content: "Bob-only trip note: mountain pass", Scope: MemoryScope{UserID: "bob"}},
	)

	aware := NewMemoryAwareMemoryCompat(inner, lt, rt, LongTermConfig{
		Enabled:          true,
		ScopeEnabled:     true,
		MaxEntries:       20,
		PinnedCategories: nil,
		RecallCategories: []MemoryCategory{adv},
		RecallPartitions: map[MemoryCategory][]MemoryPartition{
			adv: {PartitionUser, PartitionGlobal},
		},
	})
	aware.SetScope(&MemoryScope{UserID: "alice"})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	sys := msgs[0].Content()
	if !strings.Contains(sys, "glowing mushrooms") {
		t.Fatalf("alice should recall shared global adventure: %q", sys)
	}
	if !strings.Contains(sys, "secret lake") || !strings.Contains(sys, "Alice-only") {
		t.Fatalf("alice should recall her own adventure row: %q", sys)
	}
	if strings.Contains(sys, "mountain pass") {
		t.Fatalf("bob's private adventure must not leak: %q", sys)
	}
}
