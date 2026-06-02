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

func TestVerifyOneAnswer_IncludesAnswerContextBody(t *testing.T) {
	verifier := &fakeVerifierLLM{response: `{
		"verdict":"wrong_temporal_numeric_reasoning",
		"suggested_fix":"context_rendering",
		"supporting_ranks":[1],
		"strong_evidence_ranks":[1],
		"reason":"The structured answer context already contains the count candidate."
	}`}
	rec := AnswerReplayRecord{
		QID:         "q1",
		Query:       "How many tickets did Alice buy?",
		GoldAnswers: []string{"2"},
		AnswerBody:  "EVIDENCE_PACKAGE:\n  count_answer_candidate:\n    value: \"2\"\n",
		Outcome: AnswerReplayOutcome{
			Prediction: "Alice bought 3 tickets.",
			Judge:      0,
		},
		RecallArtifacts: []AnswerReplayArtifact{{Rank: 1, Content: "Alice bought 2 tickets."}},
	}
	out := verifyOneAnswer(context.Background(), verifier, rec, 0)
	if out.Error != "" {
		t.Fatalf("unexpected verifier error: %s", out.Error)
	}
	for _, want := range []string{
		"ANSWER_CONTEXT_BODY:",
		"count_answer_candidate",
		`value: "2"`,
		"TOP MEMORIES:",
	} {
		if !strings.Contains(verifier.seen, want) {
			t.Fatalf("verifier prompt missing %q:\n%s", want, verifier.seen)
		}
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
	out := answerReplayRecordsForVerification(report, replays, answerReplayFilter{OnlyMisses: true})
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

func TestAnswerReplayRecordsForVerification_FiltersBySecondaryMissAndTag(t *testing.T) {
	report := &Report{PerQuestion: []QuestionScore{
		{ID: "temporal", Query: "q1", Judge: 0, Tags: []string{"temporal"}},
		{ID: "surface", Query: "q2", Judge: 0, Tags: []string{"single-hop"}},
		{ID: "ok", Query: "q3", Judge: 0, Tags: []string{"temporal"}},
	}}
	replays := map[string]AnswerReplayRecord{
		"temporal": {QID: "temporal"},
		"surface":  {QID: "surface"},
		"ok":       {QID: "ok"},
	}
	out := answerReplayRecordsForVerification(report, replays, answerReplayFilter{
		OnlyMisses:      true,
		Tags:            csvSet("temporal"),
		SecondaryMisses: csvSet("answer_miss_temporal_or_numeric_reasoning"),
		AuditRows: map[string]answerAuditRow{
			"temporal": {QID: "temporal", SecondaryMiss: "answer_miss_temporal_or_numeric_reasoning"},
			"surface":  {QID: "surface", SecondaryMiss: "answer_miss_gold_surface_missing"},
		},
	})
	if len(out) != 1 || out[0].QID != "temporal" {
		t.Fatalf("filtered replays = %+v", out)
	}
}
