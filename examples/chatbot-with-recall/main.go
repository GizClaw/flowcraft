// Command chatbot-with-recall is a self-contained demo of how to compose
// sdk/history (conversation transcript) with sdk/recall (long-term fact
// memory) without any framework glue.
//
// The interesting bit is the chat() loop: before invoking the LLM, the
// caller asks recall.Memory for the top-K most relevant facts and
// prepends them as a system-prompt block. After the assistant replies,
// the new turn is durably appended to history and (asynchronously)
// extracted by recall to harvest any new facts.
//
// This pattern replaces the deleted MemoryAwareMemory wrapper from
// pre-v0.2.0 builds. It is ~80 lines, has no hidden state, and lets the
// caller decide how aggressive to be about recall (top-K, threshold,
// whether to inject at all on every turn).
//
// Usage: `go run .` from this directory. The demo uses an in-process
// stub LLM and an in-memory retrieval index so it has no network
// dependency. To plug in a real provider, swap stubLLM for any
// implementation of sdk/llm.LLM (e.g. sdkx/llm/openai).
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func main() {
	ctx := context.Background()

	// 1. Wire dependencies. Both sdk/history and sdk/recall are
	//    deliberately independent: nothing here imports the other
	//    package's concepts.
	hist := history.NewBufferMemory(history.NewInMemoryStore(), 20)

	mem, err := recall.New(memidx.New(),
		recall.WithLLM(&factExtractorLLM{}),
		recall.WithRequireUserID(),
		recall.WithoutSoftMerge(), // tiny demo corpus → soft-merge would noisily collapse facts
	)
	if err != nil {
		panic(err)
	}
	defer mem.Close()

	chatLLM := &chatLLM{}
	scope := recall.Scope{RuntimeID: "demo", UserID: "alice"}
	convID := "session-1"

	// 2. Drive a 4-turn conversation. Turn 1 establishes a fact, turn
	//    3 references it implicitly to show that recall picks it up.
	turns := []string{
		"My name is Alice and I prefer dark mode.",
		"What is your refund policy?",
		"What did I tell you about UI preferences?",
		"Thanks, that's all.",
	}
	for i, userMsg := range turns {
		fmt.Printf("\n--- Turn %d ---\nUSER: %s\n", i+1, userMsg)
		reply, err := chat(ctx, hist, mem, chatLLM, scope, convID, userMsg)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			return
		}
		fmt.Printf("BOT:  %s\n", reply)
	}
}

// chat is the canonical "history + recall + LLM" coordinator. The
// shape of this function is what callers used to get for free from
// MemoryAwareMemory; making it explicit lets each application tune
// when to recall, how to format hits, and how to handle extraction
// failures without the wrapper deciding for them.
func chat(
	ctx context.Context,
	hist history.Memory,
	mem recall.Memory,
	chatLLM llm.LLM,
	scope recall.Scope,
	convID, userText string,
) (string, error) {
	userMsg := model.NewTextMessage(model.RoleUser, userText)

	// (a) Pull the recent transcript so the assistant sees the running
	//     dialogue.
	transcript, err := hist.Load(ctx, convID)
	if err != nil {
		return "", fmt.Errorf("history load: %w", err)
	}

	// (b) Recall top-K relevant facts for the new user turn. Empty
	//     hits are fine — we just skip the system block in that case.
	hits, err := mem.Recall(ctx, scope, recall.RecallRequest{Query: userText, TopK: 5})
	if err != nil {
		return "", fmt.Errorf("recall: %w", err)
	}

	// (c) Build the prompt: optional [Long-term memory] block + user
	//     transcript so far + the new user turn.
	var prompt []llm.Message
	if sys := formatRecallBlock(hits); sys != "" {
		prompt = append(prompt, model.NewTextMessage(model.RoleSystem, sys))
	}
	prompt = append(prompt, transcript...)
	prompt = append(prompt, userMsg)

	// (d) Call the LLM and persist BOTH new turns at once so the next
	//     hist.Load returns them in order.
	resp, _, err := chatLLM.Generate(ctx, prompt)
	if err != nil {
		return "", fmt.Errorf("llm generate: %w", err)
	}
	if err := hist.Append(ctx, convID, []model.Message{userMsg, resp}); err != nil {
		return "", fmt.Errorf("history append: %w", err)
	}

	// (e) Harvest any new facts from this exchange. Sync Save blocks
	//     until extraction + upsert finish, which is what the demo
	//     wants so a fact taught on turn N is recallable on turn N+1.
	//     Production callers usually want SaveAsync (returns
	//     immediately, retries on failure via the configured
	//     JobQueue) — pair it with sdkx/recall/jobqueue/sqlite for
	//     crash-recoverable persistence.
	if _, err := mem.Save(ctx, scope, []llm.Message{userMsg, resp}); err != nil {
		fmt.Printf("WARN: Save failed: %v\n", err)
	}

	return resp.Content(), nil
}

func formatRecallBlock(hits []recall.RecallHit) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Long-term memory]\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "- [%s] %s\n", h.Entry.Category, h.Entry.Content)
	}
	b.WriteString("[End of long-term memory]")
	return b.String()
}

// -----------------------------------------------------------------------------
// Stub providers (replace with sdkx/llm/* in real applications)
// -----------------------------------------------------------------------------

// chatLLM mimics the chat-completion side. It surfaces the contents of
// any [Long-term memory] system block so the demo can prove that recall
// actually reaches the model.
type chatLLM struct{}

func (l *chatLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	var sysBlock, userText string
	for _, m := range msgs {
		switch m.Role {
		case model.RoleSystem:
			sysBlock = m.Content()
		case model.RoleUser:
			userText = m.Content() // keep last user turn
		}
	}
	reply := "Sure — happy to help."
	if strings.Contains(strings.ToLower(userText), "refund") {
		reply = "Our refund policy is full refund within 30 days."
	} else if strings.Contains(strings.ToLower(userText), "preference") || strings.Contains(strings.ToLower(userText), "ui") {
		if strings.Contains(sysBlock, "dark mode") {
			reply = "You told me earlier you prefer dark mode."
		} else {
			reply = "I do not have any UI preferences on file for you."
		}
	}
	return model.NewTextMessage(model.RoleAssistant, reply), llm.TokenUsage{}, nil
}

func (l *chatLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, fmt.Errorf("stream not supported in demo")
}

// factExtractorLLM is the LLM the recall extractor calls under the
// hood. It returns a tiny JSON array of facts whenever the user turn
// mentions a name or a stated preference. Real applications would
// point this at an instruction-tuned model and let the default
// AdditiveExtractor prompt do the work.
type factExtractorLLM struct{}

func (l *factExtractorLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	var convo strings.Builder
	for _, m := range msgs {
		convo.WriteString(m.Content())
		convo.WriteString("\n")
	}
	text := strings.ToLower(convo.String())
	var facts []string
	if strings.Contains(text, "my name is alice") {
		facts = append(facts, `{"content":"User name is Alice","categories":["profile"],"entities":["Alice"],"confidence":0.95}`)
	}
	if strings.Contains(text, "dark mode") {
		facts = append(facts, `{"content":"User prefers dark mode UI","categories":["preferences"],"entities":["dark mode"],"confidence":0.92}`)
	}
	body := "[" + strings.Join(facts, ",") + "]"
	return model.NewTextMessage(model.RoleAssistant, body), llm.TokenUsage{}, nil
}

func (l *factExtractorLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, fmt.Errorf("stream not supported in demo")
}
