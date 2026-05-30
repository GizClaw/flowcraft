package locomo

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

type fakeVerifierLLM struct {
	response string
	seen     string
}

func (f *fakeVerifierLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if len(msgs) > 1 {
		f.seen = msgs[1].Content()
	}
	return llm.NewTextMessage(model.RoleAssistant, f.response), llm.TokenUsage{}, nil
}

func (f *fakeVerifierLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

func TestVerifyOneAnswer_ClassifiesIgnoredEvidence(t *testing.T) {
	verifier := &fakeVerifierLLM{response: `{
		"verdict":"ignored_strong_evidence",
		"suggested_fix":"answer_reasoning",
		"supporting_ranks":[1],
		"strong_evidence_ranks":[1],
		"reason":"Memory #1 directly says Alice bought 2 tickets, but the prediction says 3."
	}`}
	rec := AnswerReplayRecord{
		QID:         "q1",
		Query:       "How many tickets did Alice buy?",
		GoldAnswers: []string{"2"},
		Outcome: AnswerReplayOutcome{
			Prediction: "Alice bought 3 tickets.",
			Judge:      0,
		},
		RecallArtifacts: []AnswerReplayArtifact{{
			Rank:    1,
			Content: "Alice bought 2 tickets.",
		}},
	}
	out := verifyOneAnswer(context.Background(), verifier, rec, 0)
	if out.Error != "" {
		t.Fatalf("unexpected verifier error: %s", out.Error)
	}
	if out.Verdict != "ignored_strong_evidence" || out.SuggestedFix != "answer_reasoning" {
		t.Fatalf("verifier output = %+v", out)
	}
	if len(out.StrongEvidenceRanks) != 1 || out.StrongEvidenceRanks[0] != 1 {
		t.Fatalf("strong evidence ranks = %+v", out.StrongEvidenceRanks)
	}
	if !strings.Contains(verifier.seen, "[#1]\nAlice bought 2 tickets.") {
		t.Fatalf("verifier prompt missing memory content:\n%s", verifier.seen)
	}
}

func TestAnswerReplayRecordsForVerification_OnlyMisses(t *testing.T) {
	report := &Report{PerQuestion: []QuestionScore{
		{ID: "ok", Judge: 1, Prediction: "right"},
		{ID: "miss", Query: "q", Judge: 0, Prediction: "wrong", Tags: []string{"temporal"}},
	}}
	replays := map[string]AnswerReplayRecord{
		"ok":   {QID: "ok"},
		"miss": {QID: "miss"},
	}
	out := answerReplayRecordsForVerification(report, replays, true, 0)
	if len(out) != 1 || out[0].QID != "miss" {
		t.Fatalf("filtered replays = %+v", out)
	}
	if out[0].Outcome.Prediction != "wrong" || out[0].Outcome.Judge != 0 {
		t.Fatalf("report outcome should refresh replay outcome: %+v", out[0].Outcome)
	}
	if len(out[0].Tags) != 1 || out[0].Tags[0] != "temporal" {
		t.Fatalf("report tags should refresh replay tags: %+v", out[0].Tags)
	}
}
