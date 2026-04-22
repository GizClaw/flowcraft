package recall

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// --- P0: Offline recall quality tests ( Phase 3) ---
//
// All tests target the canonical RetrievalLongTermStore + pipeline.LTM stack.
// Per-category time-decay (events vs profile half-life) lives in pipeline
// stages and is exercised in sdk/retrieval/pipeline tests; here we only
// assert the contract surfaced through LongTermStore.Search.

type recallCase struct {
	name        string
	entries     []*MemoryEntry
	query       string
	opts        SearchOptions
	wantHitIDs  []string
	wantMissIDs []string
	wantFirst   string
}

func runRecallCases(t *testing.T, cases []recallCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewRetrievalStore(memidx.New())
			ctx := context.Background()
			scope := MemoryScope{RuntimeID: "r1", UserID: "u1"}
			for _, e := range tc.entries {
				if e.Scope.RuntimeID == "" {
					e.Scope = scope
				}
				if err := s.Save(ctx, "r1", e); err != nil {
					t.Fatalf("save %s: %v", e.ID, err)
				}
			}

			opts := tc.opts
			if opts.Scope == nil {
				opts.Scope = &scope
			}
			results, err := s.Search(ctx, "r1", tc.query, opts)
			if err != nil {
				t.Fatal(err)
			}

			resultIDs := make(map[string]bool, len(results))
			for _, r := range results {
				resultIDs[r.ID] = true
			}

			for _, id := range tc.wantHitIDs {
				if !resultIDs[id] {
					t.Errorf("expected %q in results, got %v", id, resultIDList(results))
				}
			}
			for _, id := range tc.wantMissIDs {
				if resultIDs[id] {
					t.Errorf("expected %q NOT in results, but found", id)
				}
			}
			if tc.wantFirst != "" && len(results) > 0 && results[0].ID != tc.wantFirst {
				t.Errorf("expected %q first, got %q", tc.wantFirst, results[0].ID)
			}
		})
	}
}

func resultIDList(entries []*MemoryEntry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

func TestRecall_KeywordMatch(t *testing.T) {
	now := time.Now()
	runRecallCases(t, []recallCase{
		{
			name: "exact keyword hit",
			entries: []*MemoryEntry{
				{ID: "go-dev", Category: CategoryProfile, Content: "User is a Go developer", Keywords: []string{"go", "developer"}, UpdatedAt: now},
				{ID: "react-dev", Category: CategoryProfile, Content: "User knows React and TypeScript", Keywords: []string{"react", "typescript"}, UpdatedAt: now},
			},
			query:       "Go programming",
			opts:        SearchOptions{TopK: 5},
			wantHitIDs:  []string{"go-dev"},
			wantMissIDs: []string{"react-dev"},
		},
		{
			name: "CJK keyword hit",
			entries: []*MemoryEntry{
				{ID: "concur", Category: CategoryCases, Content: "Go 并发编程最佳实践", Keywords: []string{"go", "并发"}, UpdatedAt: now},
				{ID: "hooks", Category: CategoryCases, Content: "React hooks tutorial", Keywords: []string{"react", "hooks"}, UpdatedAt: now},
			},
			query:       "并发编程",
			opts:        SearchOptions{TopK: 5},
			wantHitIDs:  []string{"concur"},
			wantMissIDs: []string{"hooks"},
		},
		{
			name: "mixed CJK and English",
			entries: []*MemoryEntry{
				{ID: "mix", Category: CategoryCases, Content: "使用 Docker 部署 Go 服务", Keywords: []string{"docker", "go", "部署"}, UpdatedAt: now},
				{ID: "unrelated", Category: CategoryCases, Content: "Python data analysis notebook", Keywords: []string{"python"}, UpdatedAt: now},
			},
			query:       "Docker 部署",
			opts:        SearchOptions{TopK: 5},
			wantHitIDs:  []string{"mix"},
			wantMissIDs: []string{"unrelated"},
		},
		{
			name: "multi-keyword ranking",
			entries: []*MemoryEntry{
				{ID: "partial", Category: CategoryEvents, Content: "deployed microservice", Keywords: []string{"deploy"}, UpdatedAt: now},
				{ID: "full", Category: CategoryEvents, Content: "deployed Go microservice to k8s cluster", Keywords: []string{"deploy", "go", "k8s"}, UpdatedAt: now},
			},
			query:     "deploy Go k8s",
			opts:      SearchOptions{TopK: 5},
			wantFirst: "full",
		},
		{
			name: "no match returns empty",
			entries: []*MemoryEntry{
				{ID: "py", Category: CategoryProfile, Content: "Python is awesome", Keywords: []string{"python"}, UpdatedAt: now},
			},
			query:       "golang concurrency goroutine",
			opts:        SearchOptions{TopK: 5},
			wantMissIDs: []string{"py"},
		},
	})
}

func TestRecall_TimeDecayRanking(t *testing.T) {
	now := time.Now()
	runRecallCases(t, []recallCase{
		{
			name: "recent event ranks above old event with same keywords",
			entries: []*MemoryEntry{
				{ID: "old-deploy", Category: CategoryEvents, Content: "deployed service alpha to production", Keywords: []string{"deploy", "alpha"}, UpdatedAt: now.Add(-90 * 24 * time.Hour)},
				{ID: "new-deploy", Category: CategoryEvents, Content: "deployed service alpha to production", Keywords: []string{"deploy", "alpha"}, UpdatedAt: now.Add(-1 * 24 * time.Hour)},
			},
			query:     "deploy alpha",
			opts:      SearchOptions{Category: CategoryEvents, TopK: 5},
			wantFirst: "new-deploy",
		},
		{
			name: "very old event suppressed by recency",
			entries: []*MemoryEntry{
				{ID: "ancient", Category: CategoryEvents, Content: "fixed critical bug in auth service", Keywords: []string{"bug", "auth"}, UpdatedAt: now.Add(-365 * 24 * time.Hour)},
				{ID: "recent", Category: CategoryEvents, Content: "fixed auth redirect bug", Keywords: []string{"bug", "auth"}, UpdatedAt: now.Add(-2 * 24 * time.Hour)},
			},
			query:     "auth bug",
			opts:      SearchOptions{Category: CategoryEvents, TopK: 5},
			wantFirst: "recent",
		},
	})
}

func TestRecall_CategoryFiltering(t *testing.T) {
	now := time.Now()
	runRecallCases(t, []recallCase{
		{
			name: "search within specific category",
			entries: []*MemoryEntry{
				{ID: "prof", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: now},
				{ID: "ev", Category: CategoryEvents, Content: "Go meetup last week", Keywords: []string{"go", "meetup"}, UpdatedAt: now},
			},
			query:       "Go",
			opts:        SearchOptions{Category: CategoryEvents, TopK: 5},
			wantHitIDs:  []string{"ev"},
			wantMissIDs: []string{"prof"},
		},
		{
			name: "search all categories when unspecified",
			entries: []*MemoryEntry{
				{ID: "prof", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: now},
				{ID: "ev", Category: CategoryEvents, Content: "Go conference attended", Keywords: []string{"go", "conference"}, UpdatedAt: now},
			},
			query:      "Go",
			opts:       SearchOptions{TopK: 10},
			wantHitIDs: []string{"prof", "ev"},
		},
	})
}

// --- P0: Tiered injection quality tests (aware.go) ---

func TestTieredInjection_PinnedAlwaysPresent(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "write a sorting algorithm"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile":     {{ID: "p1", Category: CategoryProfile, Content: "User is a senior Go developer"}},
			"preferences": {{ID: "pref1", Category: CategoryPreferences, Content: "Prefers functional style"}},
			"entities":    {{ID: "ent1", Category: CategoryEntities, Content: "Project Falcon uses gRPC"}},
			"events":      {{ID: "ev1", Category: CategoryEvents, Content: "Deployed v3 yesterday"}},
			"cases":       {{ID: "case1", Category: CategoryCases, Content: "Solved OOM by switching to streaming"}},
			"patterns":    {{ID: "pat1", Category: CategoryPatterns, Content: "Always add context.Context to functions"}},
		},
	}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "u1", LongTermConfig{Enabled: true, MaxEntries: 20})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}

	system := msgs[0].Content()

	for _, want := range []string{"senior Go developer", "functional style"} {
		if !strings.Contains(system, want) {
			t.Errorf("pinned content %q missing from system prompt", want)
		}
	}
	for _, want := range []string{"Deployed v3", "Project Falcon", "OOM", "context.Context"} {
		if !strings.Contains(system, want) {
			t.Errorf("recalled content %q missing from system prompt", want)
		}
	}
}

func TestTieredInjection_EmptyQueryOnlyPinned(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleSystem, "system"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile": {{ID: "p1", Category: CategoryProfile, Content: "User is Alice"}},
			"events":  {{ID: "ev1", Category: CategoryEvents, Content: "Sprint review tomorrow"}},
		},
	}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "u1", LongTermConfig{Enabled: true})
	msgs, _ := aware.Load(ctx, "c1")
	system := msgs[0].Content()

	if !strings.Contains(system, "Alice") {
		t.Error("pinned profile should be present even without query")
	}
	if strings.Contains(system, "Sprint review") {
		t.Error("recall categories should NOT appear when query is empty")
	}
}

func TestTieredInjection_MaxEntriesBudget(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "anything"),
	})

	entries := map[string][]*MemoryEntry{
		"profile":     make([]*MemoryEntry, 0, 4),
		"preferences": make([]*MemoryEntry, 0, 4),
		"events":      make([]*MemoryEntry, 0, 5),
	}
	for i := 0; i < 4; i++ {
		entries["profile"] = append(entries["profile"], &MemoryEntry{
			ID: fmt.Sprintf("p%d", i), Category: CategoryProfile, Content: fmt.Sprintf("profile fact %d anything", i),
		})
		entries["preferences"] = append(entries["preferences"], &MemoryEntry{
			ID: fmt.Sprintf("pref%d", i), Category: CategoryPreferences, Content: fmt.Sprintf("preference %d anything", i),
		})
	}
	for i := 0; i < 5; i++ {
		entries["events"] = append(entries["events"], &MemoryEntry{
			ID: fmt.Sprintf("ev%d", i), Category: CategoryEvents, Content: fmt.Sprintf("event %d anything", i),
		})
	}
	ltStore := &inMemoryLTStore{entries: entries}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "u1", LongTermConfig{Enabled: true, MaxEntries: 10})
	msgs, _ := aware.Load(ctx, "c1")
	system := msgs[0].Content()

	for i := 0; i < 4; i++ {
		if !strings.Contains(system, fmt.Sprintf("profile fact %d", i)) {
			t.Errorf("pinned profile %d missing", i)
		}
		if !strings.Contains(system, fmt.Sprintf("preference %d", i)) {
			t.Errorf("pinned preference %d missing", i)
		}
	}
	eventCount := 0
	for i := 0; i < 5; i++ {
		if strings.Contains(system, fmt.Sprintf("event %d", i)) {
			eventCount++
		}
	}
	if eventCount > 3 {
		t.Errorf("recall should be budget-limited, got %d events (expected ≤3)", eventCount)
	}
}

func TestTieredInjection_NoDuplicateIDs(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "Go developer"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile":  {{ID: "shared", Category: CategoryProfile, Content: "Go developer"}},
			"entities": {{ID: "shared", Category: CategoryEntities, Content: "Go developer"}},
		},
	}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "u1", LongTermConfig{Enabled: true})
	msgs, _ := aware.Load(ctx, "c1")
	system := msgs[0].Content()

	count := strings.Count(system, "Go developer")
	if count > 1 {
		t.Errorf("duplicate entry injected %d times, expected 1", count)
	}
}
