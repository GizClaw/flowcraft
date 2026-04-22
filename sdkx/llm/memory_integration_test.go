//go:build integration

package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/memory/ltm"
	retmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// ---------------------------------------------------------------------------
// Shared helpers ( Phase 3 — extraction goes through ltm.Memory)
// ---------------------------------------------------------------------------

// expectedMemory describes one expected extraction result (soft-match).
type expectedMemory struct {
	category    memory.MemoryCategory
	mustContain []string
}

func newLTM(t *testing.T, l llm.LLM) (ltm.Memory, *memory.RetrievalLongTermStore) {
	t.Helper()
	idx := retmem.New()
	store := memory.NewRetrievalLongTermStore(idx)
	mem, err := ltm.New(ltm.Config{
		Index:           idx,
		LLM:             l,
		Mode:            ltm.ModeAdditive,
		MaxFactsPerCall: 8,
	})
	if err != nil {
		t.Fatalf("ltm.New: %v", err)
	}
	return mem, store
}

// checkExtractions verifies that stored entries satisfy expectations.
func checkExtractions(
	t *testing.T,
	store memory.LongTermStore,
	scope memory.MemoryScope,
	expects []expectedMemory,
) (hits, misses int, extras []string) {
	t.Helper()
	ctx := context.Background()
	all, _ := store.List(ctx, scope.RuntimeID, memory.ListOptions{Limit: 100, Scope: &scope})

	byCat := make(map[memory.MemoryCategory][]string)
	for _, e := range all {
		byCat[e.Category] = append(byCat[e.Category], e.Content)
	}

	expectedCats := make(map[memory.MemoryCategory]bool)
	for _, exp := range expects {
		expectedCats[exp.category] = true
		contents := byCat[exp.category]
		if len(contents) == 0 {
			misses++
			t.Errorf("  MISS category=%s: no entries extracted (wanted %v)", exp.category, exp.mustContain)
			continue
		}
		allFound := true
		for _, kw := range exp.mustContain {
			found := false
			for _, c := range contents {
				if strings.Contains(c, kw) {
					found = true
					break
				}
			}
			if !found {
				allFound = false
				t.Errorf("  MISS category=%s keyword=%q not found in %v", exp.category, kw, contents)
			}
		}
		if allFound {
			hits++
		}
	}

	for cat := range byCat {
		if !expectedCats[cat] {
			extras = append(extras, string(cat))
		}
	}
	return
}

// ---------------------------------------------------------------------------
// P2a: Extraction quality
// ---------------------------------------------------------------------------

func TestExtractQuality(t *testing.T) {
	type extractCase struct {
		name     string
		messages []llm.Message
		expects  []expectedMemory
	}

	cases := []extractCase{
		{
			name: "profile extraction",
			messages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "我叫小明，是一名 Go 后端工程师，目前在字节跳动工作"),
				llm.NewTextMessage(llm.RoleAssistant, "你好小明！很高兴认识你。"),
			},
			expects: []expectedMemory{
				{category: memory.CategoryProfile, mustContain: []string{"小明"}},
			},
		},
		{
			name: "preferences extraction",
			messages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "我习惯用 Vim 写代码，偏好暗色主题，请用中文回复我"),
				llm.NewTextMessage(llm.RoleAssistant, "好的，我会用中文回复你。"),
			},
			expects: []expectedMemory{
				{category: memory.CategoryPreferences, mustContain: []string{"Vim"}},
			},
		},
		{
			name: "entities extraction",
			messages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "我们的项目叫 Falcon，后端用 gRPC 通信，部署在 K8s 集群上"),
				llm.NewTextMessage(llm.RoleAssistant, "了解，Falcon 项目使用了 gRPC 和 K8s。"),
			},
			expects: []expectedMemory{
				{category: memory.CategoryEntities, mustContain: []string{"Falcon"}},
			},
		},
		{
			name: "events extraction",
			messages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "昨天我们上线了 v3.0 版本，修复了一个登录认证的严重 bug"),
				llm.NewTextMessage(llm.RoleAssistant, "恭喜上线！认证 bug 修复了就好。"),
			},
			expects: []expectedMemory{
				{category: memory.CategoryEvents, mustContain: []string{"v3"}},
			},
		},
		{
			name: "cases extraction",
			messages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "上次我们的服务遇到 OOM 问题，我通过把批量处理改成流式处理解决了，内存降了 80%"),
				llm.NewTextMessage(llm.RoleAssistant, "流式处理是个好方案，能显著降低内存占用。"),
			},
			expects: []expectedMemory{
				{category: memory.CategoryCases, mustContain: []string{"OOM"}},
			},
		},
		{
			name: "patterns extraction",
			messages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "我总结了一个经验：写 Go 代码一定要传 context.Context，之前因为漏掉导致超时无法取消"),
				llm.NewTextMessage(llm.RoleAssistant, "这是个很好的实践，context 是 Go 并发控制的关键。"),
			},
			expects: []expectedMemory{
				{category: memory.CategoryPatterns, mustContain: []string{"context"}},
			},
		},
	}

	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					mem, store := newLTM(t, provider)
					defer mem.Close()

					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()

					scope := memory.MemoryScope{RuntimeID: "test-rt", UserID: "u1"}
					if _, err := mem.Save(ctx, scope, tc.messages); err != nil {
						t.Fatalf("Save failed: %v", err)
					}

					hits, misses, extras := checkExtractions(t, store, scope, tc.expects)
					t.Logf("hits=%d misses=%d extras=%v", hits, misses, extras)

					all, _ := store.List(ctx, "test-rt", memory.ListOptions{Limit: 50, Scope: &scope})
					for _, e := range all {
						t.Logf("  [%s] %s (kw: %v)", e.Category, e.Content, e.Keywords)
					}
				})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// P2a: Deduplication quality
// ---------------------------------------------------------------------------

func TestDeduplicationQuality(t *testing.T) {
	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			mem, store := newLTM(t, provider)
			defer mem.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			scope := memory.MemoryScope{RuntimeID: "test-rt", UserID: "u1"}

			_ = store.Save(ctx, "test-rt", &memory.MemoryEntry{
				Category: memory.CategoryProfile,
				Content:  "User is a Go backend developer",
				Keywords: []string{"go", "backend", "developer"},
				Scope:    scope,
			})

			if _, err := mem.Save(ctx, scope, []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "I'm a Go developer working on backend services"),
				llm.NewTextMessage(llm.RoleAssistant, "Great, I'll keep that in mind!"),
			}); err != nil {
				t.Fatalf("Save failed: %v", err)
			}

			entries, _ := store.List(ctx, "test-rt", memory.ListOptions{Category: memory.CategoryProfile, Limit: 50, Scope: &scope})
			t.Logf("profile entries after dedup: %d", len(entries))
			for _, e := range entries {
				t.Logf("  [%s] id=%s content=%q", e.Category, e.ID, e.Content)
			}

			if len(entries) > 4 {
				t.Errorf("expected at most 4 profile entries, got %d — dedup may have failed", len(entries))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// P2b: End-to-end effect evaluation (LLM-as-a-judge)
// ---------------------------------------------------------------------------

type judgeResult struct {
	Winner string `json:"winner"`
	Reason string `json:"reason"`
}

type e2eCase struct {
	name          string
	seedMessages  []llm.Message
	followUpQuery string
	judgeHint     string
}

func TestEndToEndMemory(t *testing.T) {
	cases := []e2eCase{
		{
			name: "remembers programming language",
			seedMessages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "我是一名 Go 后端工程师，平时主要用 Go 写服务端代码"),
				llm.NewTextMessage(llm.RoleAssistant, "了解，你主要使用 Go 语言。"),
			},
			followUpQuery: "帮我写一个快速排序的实现",
			judgeHint:     "回答是否使用了 Go 语言来实现（而不是 Python 或其他语言）",
		},
		{
			name: "remembers code style preference",
			seedMessages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "我偏好简洁的代码风格，不要过多注释，变量名尽量短"),
				llm.NewTextMessage(llm.RoleAssistant, "好的，我会注意代码风格。"),
			},
			followUpQuery: "写一个 HTTP health check handler",
			judgeHint:     "回答 B 的代码是否比 A 更简洁、注释更少",
		},
		{
			name: "remembers project tech stack",
			seedMessages: []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "我们项目用 gRPC + Protocol Buffers 做服务间通信，数据库是 PostgreSQL"),
				llm.NewTextMessage(llm.RoleAssistant, "了解你们的技术栈。"),
			},
			followUpQuery: "我需要设计一个新的用户服务 API，应该怎么设计？",
			judgeHint:     "回答 B 是否提到了 gRPC 或 Protocol Buffers 或 PostgreSQL",
		},
	}

	const judgePrompt = `You are an evaluation expert. Given a user query and two AI responses, determine which response better utilizes the user's personal information and context.

User query: %s

Evaluation focus: %s

Response A (no personal memory):
%s

Response B (with personal memory injected):
%s

Reply in JSON: {"winner":"A" or "B" or "tie", "reason":"one sentence explanation"}
Only return the JSON object, nothing else.`

	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					defer cancel()

					mem, store := newLTM(t, provider)
					defer mem.Close()

					scope := memory.MemoryScope{RuntimeID: "test-rt", UserID: "u1"}
					if _, err := mem.Save(ctx, scope, tc.seedMessages); err != nil {
						t.Fatalf("Seed save failed: %v", err)
					}

					extracted, _ := store.List(ctx, "test-rt", memory.ListOptions{Limit: 50, Scope: &scope})
					t.Logf("extracted %d entries:", len(extracted))
					for _, e := range extracted {
						t.Logf("  [%s] %s", e.Category, e.Content)
					}
					if len(extracted) == 0 {
						t.Fatal("no memories extracted, cannot proceed with e2e test")
					}

					msgsWithout := []llm.Message{
						llm.NewTextMessage(llm.RoleSystem, "You are a helpful programming assistant. Reply in Chinese."),
						llm.NewTextMessage(llm.RoleUser, tc.followUpQuery),
					}
					genOpts := []llm.GenerateOption{llm.WithMaxTokens(500), llm.WithTemperature(0.3)}
					respA, _, err := provider.Generate(ctx, msgsWithout, genOpts...)
					if err != nil {
						t.Fatalf("Generate A failed: %v", err)
					}
					answerA := respA.Content()

					msgStore := memory.NewInMemoryStore()
					_ = msgStore.SaveMessages(ctx, "followup", []llm.Message{
						llm.NewTextMessage(llm.RoleSystem, "You are a helpful programming assistant. Reply in Chinese."),
						llm.NewTextMessage(llm.RoleUser, tc.followUpQuery),
					})
					inner := memory.NewBufferMemory(msgStore, 50)
					aware := memory.NewMemoryAwareMemoryCompat(inner, store, "test-rt", memory.LongTermConfig{Enabled: true, MaxEntries: 10})
					aware.SetScope(&scope)
					msgsWith, err := aware.Load(ctx, "followup")
					if err != nil {
						t.Fatalf("Load with memory failed: %v", err)
					}

					respB, _, err := provider.Generate(ctx, msgsWith, genOpts...)
					if err != nil {
						t.Fatalf("Generate B failed: %v", err)
					}
					answerB := respB.Content()

					t.Logf("--- Answer A (no memory) ---\n%s", truncateForLog(answerA, 300))
					t.Logf("--- Answer B (with memory) ---\n%s", truncateForLog(answerB, 300))

					judgeMsg := fmt.Sprintf(judgePrompt,
						tc.followUpQuery, tc.judgeHint,
						truncateForLog(answerA, 800), truncateForLog(answerB, 800),
					)
					judgeResp, _, err := provider.Generate(ctx, []llm.Message{
						llm.NewTextMessage(llm.RoleUser, judgeMsg),
					}, llm.WithMaxTokens(200), llm.WithJSONMode(true))
					if err != nil {
						t.Logf("Judge call failed: %v (non-fatal)", err)
						return
					}

					var result judgeResult
					raw := strings.TrimSpace(judgeResp.Content())
					if err := json.Unmarshal([]byte(stripJSONFence(raw)), &result); err != nil {
						t.Logf("Judge parse failed: %v raw=%q (non-fatal)", err, truncateForLog(raw, 200))
						return
					}

					t.Logf("JUDGE: winner=%s reason=%q", result.Winner, result.Reason)
					if result.Winner == "A" {
						t.Logf("WARNING: memory injection did not help for this case")
					}
				})
			}
		})
	}
}

func truncateForLog(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func stripJSONFence(s string) string {
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
		if strings.HasSuffix(s, "```") {
			s = s[:len(s)-3]
		}
		s = strings.TrimSpace(s)
	}
	return s
}
