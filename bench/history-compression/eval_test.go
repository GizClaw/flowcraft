package historycompression_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	hc "github.com/GizClaw/flowcraft/bench/history-compression"
	"github.com/GizClaw/flowcraft/bench/locomo/dataset"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// echoLLM picks an answer based on the question keyword, but only when that
// answer text is also present in the prompt. This isolates "did history.Load
// keep the relevant turn?" from "does the model hallucinate?": when the
// compactor (or buffer) truncates the evidence turn, echoLLM emits IDK and
// EM falls.
type echoLLM struct{}

var qaTriggers = []struct {
	qHints  []string
	goldFor string
}{
	{qHints: []string{"live", "where"}, goldFor: "san francisco"},
	{qHints: []string{"coffee", "drink"}, goldFor: "black coffee"},
	{qHints: []string{"宠物", "狗", "pet"}, goldFor: "旺财"},
}

func (echoLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	prompt := ""
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == model.PartText {
				prompt += " " + strings.ToLower(p.Text)
			}
		}
	}
	for _, t := range qaTriggers {
		matched := false
		for _, h := range t.qHints {
			if strings.Contains(prompt, h) {
				matched = true
				break
			}
		}
		if matched && strings.Contains(prompt, t.goldFor) {
			return llm.Message{
				Role:  model.RoleAssistant,
				Parts: []model.Part{{Type: model.PartText, Text: t.goldFor}},
			}, llm.TokenUsage{}, nil
		}
	}
	return llm.Message{
		Role:  model.RoleAssistant,
		Parts: []model.Part{{Type: model.PartText, Text: "I don't know"}},
	}, llm.TokenUsage{}, nil
}

func (echoLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not used")
}

func TestRun_Synthetic(t *testing.T) {
	rep, err := hc.Run(context.Background(), dataset.Synthetic(), hc.Options{
		AnswerLLM:  echoLLM{},
		Strategies: []hc.Strategy{hc.StrategyNone, hc.StrategyBuffer},
		BufferMax:  2, // < 3-turn c1 so the second user turn is dropped
	})
	if err != nil {
		t.Fatal(err)
	}

	none, buf := rep.Strategies[hc.StrategyNone], rep.Strategies[hc.StrategyBuffer]
	if none == nil || buf == nil {
		t.Fatalf("missing report: %+v", rep.Strategies)
	}
	if none.Errors != 0 || buf.Errors != 0 {
		t.Fatalf("unexpected errors: none=%d buf=%d", none.Errors, buf.Errors)
	}
	// `none` should answer everything (3/3 questions). `buffer` truncates
	// before the "black coffee" turn → at most 2/3 right. The point of the
	// bench is exactly to surface such truncation regressions.
	if !(none.EM > buf.EM) {
		t.Fatalf("expected none.EM (%.2f) > buffer.EM (%.2f); buffer should drop earlier turns at MaxMessages=2",
			none.EM, buf.EM)
	}
	if none.PromptTokensP95 < buf.PromptTokensP95 {
		t.Fatalf("expected none.tokens (%d) >= buffer.tokens (%d)", none.PromptTokensP95, buf.PromptTokensP95)
	}
}

func TestRun_CompactedSkippedWithoutLLM(t *testing.T) {
	rep, err := hc.Run(context.Background(), dataset.Synthetic(), hc.Options{
		AnswerLLM:  echoLLM{},
		Strategies: []hc.Strategy{hc.StrategyCompacted},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := rep.Strategies[hc.StrategyCompacted]
	if r == nil || r.Skipped == "" {
		t.Fatalf("expected compacted strategy to be skipped, got %+v", r)
	}
}
