// Command chatbot-with-recall demos sdk/history + sdk/recall wiring.
// Run with: GOWORK=off go run . — see chat() for the pattern.
package main

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func main() {
	ctx := context.Background()
	hist := history.NewBuffer(history.NewInMemoryStore(), history.WithBufferMax(20))
	mem, err := recall.New(memidx.New(),
		recall.WithLLM(&factExtractorLLM{}),
		recall.WithRequireUserID(),
		recall.WithoutSoftMerge(),
	)
	if err != nil {
		panic(err)
	}
	defer mem.Close()

	scope := recall.Scope{RuntimeID: "demo", UserID: "alice"}
	for i, u := range demoTurns {
		fmt.Printf("\n--- Turn %d ---\nUSER: %s\n", i+1, u)
		r, err := chat(ctx, hist, mem, &chatLLM{}, scope, "session-1", u)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			return
		}
		fmt.Printf("BOT:  %s\n", r)
	}
}

// chat is the canonical "history + recall + LLM" coordinator: load
// transcript, recall relevant facts, inject as system prompt, call
// LLM, persist the new turn, harvest new facts.
func chat(ctx context.Context, hist history.Memory, mem recall.Memory, chatLLM llm.LLM, scope recall.Scope, convID, userText string) (string, error) {
	userMsg := model.NewTextMessage(model.RoleUser, userText)
	transcript, err := hist.Load(ctx, convID, history.Budget{})
	if err != nil {
		return "", err
	}
	hits, err := mem.Recall(ctx, scope, recall.Request{Query: userText, TopK: 5})
	if err != nil {
		return "", err
	}
	prompt := buildPrompt(hits, transcript, userMsg)
	resp, _, err := chatLLM.Generate(ctx, prompt)
	if err != nil {
		return "", err
	}
	if err := hist.Append(ctx, convID, []model.Message{userMsg, resp}); err != nil {
		return "", err
	}
	// Sync Save for demo; production → SaveAsync + sdkx/recall/jobqueue/sqlite.
	_, _ = mem.Save(ctx, scope, []llm.Message{userMsg, resp})
	return resp.Content(), nil
}

