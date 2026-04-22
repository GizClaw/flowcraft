package recall

import (
	"context"
	"testing"
	"time"

	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// These tests assert the quality surface of Memory.Recall: keyword
// match, per-category time-decay ranking, and category filtering via
// req.Filter. Per-category time-decay itself is unit-tested in
// sdk/retrieval/pipeline; here we only confirm it reaches callers
// through the Memory facade.

type recallCase struct {
	name        string
	entries     []Entry
	query       string
	category    Category
	topK        int
	wantHitIDs  []string
	wantMissIDs []string
	wantFirst   string
}

func runRecallCases(t *testing.T, cases []recallCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, err := New(memidx.New(), WithRequireUserID())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer m.Close()

			scope := Scope{RuntimeID: "r1", UserID: "u1"}
			for _, e := range tc.entries {
				if _, err := m.Add(ctx, scope, e); err != nil {
					t.Fatalf("add %s: %v", e.ID, err)
				}
			}

			req := Request{Query: tc.query, TopK: tc.topK}
			if tc.category != "" {
				req.Filter = map[string]any{"category": string(tc.category)}
			}
			hits, err := m.Recall(ctx, scope, req)
			if err != nil {
				t.Fatal(err)
			}

			resultIDs := make(map[string]bool, len(hits))
			ids := make([]string, len(hits))
			for i, h := range hits {
				resultIDs[h.Entry.ID] = true
				ids[i] = h.Entry.ID
			}
			for _, id := range tc.wantHitIDs {
				if !resultIDs[id] {
					t.Errorf("expected %q in results, got %v", id, ids)
				}
			}
			for _, id := range tc.wantMissIDs {
				if resultIDs[id] {
					t.Errorf("expected %q NOT in results, but found (ids=%v)", id, ids)
				}
			}
			if tc.wantFirst != "" && len(hits) > 0 && hits[0].Entry.ID != tc.wantFirst {
				t.Errorf("expected %q first, got %q (ids=%v)", tc.wantFirst, hits[0].Entry.ID, ids)
			}
		})
	}
}

func TestRecall_KeywordMatch(t *testing.T) {
	now := time.Now()
	runRecallCases(t, []recallCase{
		{
			name: "exact keyword hit",
			entries: []Entry{
				{ID: "go-dev", Category: CategoryProfile, Content: "User is a Go developer", Keywords: []string{"go", "developer"}, UpdatedAt: now},
				{ID: "react-dev", Category: CategoryProfile, Content: "User knows React and TypeScript", Keywords: []string{"react", "typescript"}, UpdatedAt: now},
			},
			query:       "Go programming",
			topK:        5,
			wantHitIDs:  []string{"go-dev"},
			wantMissIDs: []string{"react-dev"},
		},
		{
			name: "CJK keyword hit",
			entries: []Entry{
				{ID: "concur", Category: CategoryCases, Content: "Go 并发编程最佳实践", Keywords: []string{"go", "并发"}, UpdatedAt: now},
				{ID: "hooks", Category: CategoryCases, Content: "React hooks tutorial", Keywords: []string{"react", "hooks"}, UpdatedAt: now},
			},
			query:       "并发编程",
			topK:        5,
			wantHitIDs:  []string{"concur"},
			wantMissIDs: []string{"hooks"},
		},
		{
			name: "mixed CJK and English",
			entries: []Entry{
				{ID: "mix", Category: CategoryCases, Content: "使用 Docker 部署 Go 服务", Keywords: []string{"docker", "go", "部署"}, UpdatedAt: now},
				{ID: "unrelated", Category: CategoryCases, Content: "Python data analysis notebook", Keywords: []string{"python"}, UpdatedAt: now},
			},
			query:       "Docker 部署",
			topK:        5,
			wantHitIDs:  []string{"mix"},
			wantMissIDs: []string{"unrelated"},
		},
		{
			name: "multi-keyword ranking",
			entries: []Entry{
				{ID: "partial", Category: CategoryEvents, Content: "deployed microservice", Keywords: []string{"deploy"}, UpdatedAt: now},
				{ID: "full", Category: CategoryEvents, Content: "deployed Go microservice to k8s cluster", Keywords: []string{"deploy", "go", "k8s"}, UpdatedAt: now},
			},
			query:     "deploy Go k8s",
			topK:      5,
			wantFirst: "full",
		},
		{
			name: "no match returns empty",
			entries: []Entry{
				{ID: "py", Category: CategoryProfile, Content: "Python is awesome", Keywords: []string{"python"}, UpdatedAt: now},
			},
			query:       "golang concurrency goroutine",
			topK:        5,
			wantMissIDs: []string{"py"},
		},
	})
}

func TestRecall_TimeDecayRanking(t *testing.T) {
	now := time.Now()
	runRecallCases(t, []recallCase{
		{
			name: "recent event ranks above old event with same keywords",
			entries: []Entry{
				{ID: "old-deploy", Category: CategoryEvents, Content: "deployed service alpha to production", Keywords: []string{"deploy", "alpha"}, UpdatedAt: now.Add(-90 * 24 * time.Hour)},
				{ID: "new-deploy", Category: CategoryEvents, Content: "deployed service alpha to production", Keywords: []string{"deploy", "alpha"}, UpdatedAt: now.Add(-1 * 24 * time.Hour)},
			},
			query:     "deploy alpha",
			category:  CategoryEvents,
			topK:      5,
			wantFirst: "new-deploy",
		},
		{
			name: "very old event suppressed by recency",
			entries: []Entry{
				{ID: "ancient", Category: CategoryEvents, Content: "fixed critical bug in auth service", Keywords: []string{"bug", "auth"}, UpdatedAt: now.Add(-365 * 24 * time.Hour)},
				{ID: "recent", Category: CategoryEvents, Content: "fixed auth redirect bug", Keywords: []string{"bug", "auth"}, UpdatedAt: now.Add(-2 * 24 * time.Hour)},
			},
			query:     "auth bug",
			category:  CategoryEvents,
			topK:      5,
			wantFirst: "recent",
		},
	})
}

func TestRecall_CategoryFiltering(t *testing.T) {
	now := time.Now()
	runRecallCases(t, []recallCase{
		{
			name: "search within specific category",
			entries: []Entry{
				{ID: "prof", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: now},
				{ID: "ev", Category: CategoryEvents, Content: "Go meetup last week", Keywords: []string{"go", "meetup"}, UpdatedAt: now},
			},
			query:       "Go",
			category:    CategoryEvents,
			topK:        5,
			wantHitIDs:  []string{"ev"},
			wantMissIDs: []string{"prof"},
		},
		{
			name: "search all categories when unspecified",
			entries: []Entry{
				{ID: "prof", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: now},
				{ID: "ev", Category: CategoryEvents, Content: "Go conference attended", Keywords: []string{"go", "conference"}, UpdatedAt: now},
			},
			query:      "Go",
			topK:       10,
			wantHitIDs: []string{"prof", "ev"},
		},
	})
}
