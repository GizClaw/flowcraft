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

// withEvidenceDataset is a focused fixture for the truncation detector: c1
// has a 4-turn arc whose first turn carries the only evidence. BufferMax=2
// guarantees that turn falls out of the window, so StrategyBuffer should
// report Truncated=1 while StrategyNone reports 0.
func withEvidenceDataset() *dataset.Dataset {
	return &dataset.Dataset{
		Name: "with-evidence",
		Conversations: []dataset.Conversation{{
			ID: "c1",
			Turns: []dataset.Turn{
				{Role: "user", Content: "I bought a yellow Toyota Prius in 2021.", EvidenceID: "c1:1"},
				{Role: "assistant", Content: "Nice — fuel-efficient choice."},
				{Role: "user", Content: "Today is sunny in Berlin."},
				{Role: "assistant", Content: "Enjoy the weather!"},
			},
		}},
		Questions: []dataset.Question{{
			ID: "q1", ConversationID: "c1",
			Query:       "What car does the user own?",
			GoldAnswers: []string{"Prius", "Toyota Prius"},
			EvidenceIDs: []string{"c1:1"},
		}},
	}
}

func TestRun_TruncatedDetected(t *testing.T) {
	rep, err := hc.Run(context.Background(), withEvidenceDataset(), hc.Options{
		AnswerLLM:  echoLLM{},
		Strategies: []hc.Strategy{hc.StrategyNone, hc.StrategyBuffer},
		BufferMax:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	none := rep.Strategies[hc.StrategyNone]
	buf := rep.Strategies[hc.StrategyBuffer]
	if none.EvidenceMeasured != 1 || buf.EvidenceMeasured != 1 {
		t.Fatalf("EvidenceMeasured: none=%d buf=%d", none.EvidenceMeasured, buf.EvidenceMeasured)
	}
	if none.Truncated != 0 {
		t.Fatalf("StrategyNone must not truncate evidence, got %d (rate=%.2f)", none.Truncated, none.TruncatedRate)
	}
	if buf.Truncated != 1 {
		t.Fatalf("StrategyBuffer (max=2) must truncate the evidence turn, got %d", buf.Truncated)
	}
	if buf.TruncatedRate != 1.0 {
		t.Fatalf("buf.TruncatedRate=%.2f, want 1.0", buf.TruncatedRate)
	}
}

func TestRun_NoEvidenceLeavesTruncatedZero(t *testing.T) {
	// Synthetic carries no evidence_ids; the detector must report 0/0
	// rather than dividing by zero or flagging false positives.
	rep, err := hc.Run(context.Background(), dataset.Synthetic(), hc.Options{
		AnswerLLM:  echoLLM{},
		Strategies: []hc.Strategy{hc.StrategyNone},
	})
	if err != nil {
		t.Fatal(err)
	}
	none := rep.Strategies[hc.StrategyNone]
	if none.EvidenceMeasured != 0 || none.Truncated != 0 || none.TruncatedRate != 0 {
		t.Fatalf("expected all-zero evidence stats, got measured=%d truncated=%d rate=%.2f",
			none.EvidenceMeasured, none.Truncated, none.TruncatedRate)
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
