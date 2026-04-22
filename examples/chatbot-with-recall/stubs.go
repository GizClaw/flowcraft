package main

// The stub providers in this file stand in for real LLM clients (e.g.
// sdkx/llm/openai) so the demo runs offline. Swap them for any
// implementation of sdk/llm.LLM in a real application.

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

// demoTurns drives the scripted conversation in main.
var demoTurns = []string{
	"My name is Alice and I prefer dark mode.",
	"What is your refund policy?",
	"What did I tell you about UI preferences?",
	"Thanks, that's all.",
}

// buildPrompt stitches the optional recall-hits system block, the
// running transcript, and the new user turn into one LLM input slice.
func buildPrompt(hits []recall.Hit, transcript []llm.Message, user llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(transcript)+2)
	if sys := formatRecallBlock(hits); sys != "" {
		out = append(out, model.NewTextMessage(model.RoleSystem, sys))
	}
	return append(append(out, transcript...), user)
}

func formatRecallBlock(hits []recall.Hit) string {
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
	switch {
	case strings.Contains(strings.ToLower(userText), "refund"):
		reply = "Our refund policy is full refund within 30 days."
	case strings.Contains(strings.ToLower(userText), "preference"),
		strings.Contains(strings.ToLower(userText), "ui"):
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
