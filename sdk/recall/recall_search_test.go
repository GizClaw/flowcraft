package recall

import (
	"context"
	"testing"
	"time"

	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// --- P0: Offline recall quality tests ( Phase 3) ---
//
// All tests target the canonical RetrievalLongTermStore + pipeline.LTM stack.
// Per-category time-decay (events vs profile half-life) lives in pipeline
// stages and is exercised in sdk/retrieval/pipeline tests; here we only
// assert the contract surfaced through Store.Search.

type recallCase struct {
	name        string
	entries     []*Entry
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
			scope := Scope{RuntimeID: "r1", UserID: "u1"}
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

func resultIDList(entries []*Entry) []string {
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
			entries: []*Entry{
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
			entries: []*Entry{
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
			entries: []*Entry{
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
			entries: []*Entry{
				{ID: "partial", Category: CategoryEvents, Content: "deployed microservice", Keywords: []string{"deploy"}, UpdatedAt: now},
				{ID: "full", Category: CategoryEvents, Content: "deployed Go microservice to k8s cluster", Keywords: []string{"deploy", "go", "k8s"}, UpdatedAt: now},
			},
			query:     "deploy Go k8s",
			opts:      SearchOptions{TopK: 5},
			wantFirst: "full",
		},
		{
			name: "no match returns empty",
			entries: []*Entry{
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
			entries: []*Entry{
				{ID: "old-deploy", Category: CategoryEvents, Content: "deployed service alpha to production", Keywords: []string{"deploy", "alpha"}, UpdatedAt: now.Add(-90 * 24 * time.Hour)},
				{ID: "new-deploy", Category: CategoryEvents, Content: "deployed service alpha to production", Keywords: []string{"deploy", "alpha"}, UpdatedAt: now.Add(-1 * 24 * time.Hour)},
			},
			query:     "deploy alpha",
			opts:      SearchOptions{Category: CategoryEvents, TopK: 5},
			wantFirst: "new-deploy",
		},
		{
			name: "very old event suppressed by recency",
			entries: []*Entry{
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
			entries: []*Entry{
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
			entries: []*Entry{
				{ID: "prof", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: now},
				{ID: "ev", Category: CategoryEvents, Content: "Go conference attended", Keywords: []string{"go", "conference"}, UpdatedAt: now},
			},
			query:      "Go",
			opts:       SearchOptions{TopK: 10},
			wantHitIDs: []string{"prof", "ev"},
		},
	})
}

