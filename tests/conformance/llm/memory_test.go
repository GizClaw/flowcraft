package llm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
)

// TestMemoryExtractionQuality keeps provider-level coverage on the public
// temporal-memory API after the legacy sdk/recall conformance suite was
// removed.
func TestMemoryExtractionQuality(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		query    string
		contains string
	}{
		{
			name:     "profile",
			user:     "我叫小明，是一名 Go 后端工程师，目前在字节跳动工作。",
			query:    "小明的工作是什么？",
			contains: "小明",
		},
		{
			name:     "preference",
			user:     "我习惯用 Vim 写代码，偏好暗色主题，请用中文回复我。",
			query:    "用户偏好什么编辑器？",
			contains: "Vim",
		},
		{
			name:     "project",
			user:     "我们的项目叫 Falcon，后端用 gRPC 通信，部署在 K8s 集群上。",
			query:    "Falcon 项目使用什么技术？",
			contains: "Falcon",
		},
	}

	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					mem, err := recall.New(recall.WithLLMExtractor(provider))
					if err != nil {
						t.Fatalf("recall.New: %v", err)
					}
					defer mem.Close()

					ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
					defer cancel()
					scope := recall.Scope{RuntimeID: "conformance", UserID: "u1", AgentID: spec.Provider}
					save, err := mem.Save(ctx, scope, recall.SaveRequest{
						Turns: []recall.TurnContext{{
							ID:        "user-1",
							SessionID: tc.name,
							Role:      "user",
							Speaker:   "user",
							Text:      tc.user,
							Time:      time.Now(),
						}},
					})
					if err != nil {
						t.Fatalf("Save: %v", err)
					}
					if len(save.FactIDs) == 0 {
						t.Fatal("extractor returned no facts")
					}
					processor, ok := recall.NewSideEffectProcessor(mem)
					if !ok {
						t.Fatal("side-effect processor unavailable")
					}
					if _, err := processor.ProcessSideEffects(ctx, recall.SideEffectProcessOptions{
						Scope: scope,
						Limit: 100,
					}); err != nil {
						t.Fatalf("ProcessSideEffects: %v", err)
					}

					hits, err := mem.Recall(ctx, scope, recall.Query{Text: tc.query, Limit: 20})
					if err != nil {
						t.Fatalf("Recall: %v", err)
					}
					found := false
					for _, hit := range hits {
						if strings.Contains(strings.ToLower(hit.Fact.Content), strings.ToLower(tc.contains)) {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("recalled facts do not contain %q: %+v", tc.contains, hits)
					}
				})
			}
		})
	}
}
