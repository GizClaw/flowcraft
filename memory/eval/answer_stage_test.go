package eval

import (
	"context"
	"errors"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

type stubAnswerer struct {
	answer string
	err    error
}

func (answerer stubAnswerer) Answer(context.Context, Question, []corememory.ContextItem) (string, error) {
	return answerer.answer, answerer.err
}

type stubJudge struct {
	verdict bool
	err     error
}

func (judge stubJudge) Judge(context.Context, Question, string) (bool, error) {
	return judge.verdict, judge.err
}

func answerScenario() Scenario {
	return Scenario{
		Name: "answer-stage", Scope: Scope{RuntimeID: "memories"}, ConversationID: "conv-1",
		Turns: []Turn{{IdempotencyKey: "t1", Messages: []coremessage.Message{{
			Role:    coremessage.RoleUser,
			Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "Alice likes tea"}}},
		}}}},
		Questions: []Question{
			{Query: "tea", WantContains: []string{"Alice likes tea"}, Category: 1},
			{Query: "coffee", WantContains: []string{"coffee"}, Category: 2},
		},
	}
}

func TestRunWithOptionsGradesGeneratedAnswers(t *testing.T) {
	report, err := RunWithOptions(context.Background(), stubRunner{answer: "Alice likes tea"},
		answerScenario(), Options{
			Answerer: stubAnswerer{answer: "Alice likes tea"},
			Judge:    stubJudge{verdict: true},
		})
	if err != nil {
		t.Fatal(err)
	}
	if report.Hits != 1 || report.HitRate != 0.5 {
		t.Fatalf("recall = %d/%v, want 1/0.5", report.Hits, report.HitRate)
	}
	if report.Answers != 2 || report.AnswerHits != 2 || report.AnswerRate != 1 {
		t.Fatalf("answers = %d/%d/%v", report.Answers, report.AnswerHits, report.AnswerRate)
	}
	if report.ByCategory[1].Answered != 1 || report.ByCategory[1].AnswerHits != 1 ||
		report.ByCategory[2].Answered != 1 {
		t.Fatalf("category stats = %#v", report.ByCategory)
	}
	for _, question := range report.Questions {
		if !question.AnswerGraded || question.Answer != "Alice likes tea" {
			t.Fatalf("question = %#v", question)
		}
	}
}

func TestRunWithOptionsFallsBackToContainmentWithoutJudge(t *testing.T) {
	report, err := RunWithOptions(context.Background(), stubRunner{answer: "Alice likes tea"},
		answerScenario(), Options{Answerer: stubAnswerer{answer: "It was tea."}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Answers != 2 || report.AnswerHits != 0 {
		t.Fatalf("answers = %d/%d, want 2/0", report.Answers, report.AnswerHits)
	}
}

func TestRunWithOptionsRecordsBothJudges(t *testing.T) {
	report, err := RunWithOptions(context.Background(), stubRunner{answer: "Alice likes tea"},
		answerScenario(), Options{
			Answerer:     stubAnswerer{answer: "Alice likes tea"},
			Judge:        stubJudge{verdict: true},
			LenientJudge: stubJudge{verdict: false},
		})
	if err != nil {
		t.Fatal(err)
	}
	if report.Answers != 2 || report.AnswerHits != 2 || report.AnswerRate != 1 {
		t.Fatalf("strict = %d/%d/%v", report.Answers, report.AnswerHits, report.AnswerRate)
	}
	if report.LenientAnswers != 2 || report.LenientHits != 0 || report.LenientRate != 0 {
		t.Fatalf("lenient = %d/%d/%v", report.LenientAnswers, report.LenientHits, report.LenientRate)
	}
	if !report.Questions[0].LenientGraded || report.Questions[0].LenientHit {
		t.Fatalf("question = %#v", report.Questions[0])
	}
	if report.ByCategory[1].LenientAnswered != 1 {
		t.Fatalf("category = %#v", report.ByCategory[1])
	}
}

func TestRunWithOptionsPropagatesStageErrors(t *testing.T) {
	if _, err := RunWithOptions(context.Background(), stubRunner{}, answerScenario(),
		Options{Answerer: stubAnswerer{err: errors.New("boom")}}); err == nil {
		t.Fatal("answerer error was swallowed")
	}
	if _, err := RunWithOptions(context.Background(), stubRunner{}, answerScenario(),
		Options{Judge: stubJudge{verdict: true}}); err == nil {
		t.Fatal("a judge without an answerer was accepted")
	}
}

// TestRunWithOptionsLeavesUngradableAnswersOutOfTheRate pins the judge
// contract: an unusable verdict removes the answer from the denominator and is
// reported, instead of being counted as a hit (the old behaviour) or aborting a
// long run.
func TestRunWithOptionsLeavesUngradableAnswersOutOfTheRate(t *testing.T) {
	report, err := RunWithOptions(context.Background(), stubRunner{answer: "Alice likes tea"},
		answerScenario(), Options{
			Answerer: stubAnswerer{answer: "Alice likes tea"},
			Judge:    stubJudge{err: errors.New("unrecognized judge verdict")},
		})
	if err != nil {
		t.Fatal(err)
	}
	if report.Answers != 2 || report.UngradedAnswers != 2 || report.AnswerHits != 0 || report.AnswerRate != 0 {
		t.Fatalf("answers = %d/%d/%d/%v", report.Answers, report.UngradedAnswers, report.AnswerHits, report.AnswerRate)
	}
	for _, question := range report.Questions {
		if !question.AnswerUngraded || question.AnswerGraded || question.AnswerHit || question.AnswerNote == "" {
			t.Fatalf("question = %#v", question)
		}
	}
	if stats := report.ByCategory[1]; stats.Answered != 0 || stats.AnswerHits != 0 {
		t.Fatalf("category stats counted an ungraded answer = %#v", stats)
	}
}

// TestRunWithOptionsGradesHalfUngradedRun keeps the arithmetic honest: a judge
// that grades one question and fails the other yields one graded answer.
func TestRunWithOptionsGradesHalfUngradedRun(t *testing.T) {
	report, err := RunWithOptions(context.Background(), stubRunner{answer: "Alice likes tea"},
		answerScenario(), Options{
			Answerer: stubAnswerer{answer: "Alice likes tea"},
			Judge:    &alternatingJudge{},
		})
	if err != nil {
		t.Fatal(err)
	}
	if report.Answers != 2 || report.UngradedAnswers != 1 || report.AnswerHits != 1 || report.AnswerRate != 1 {
		t.Fatalf("answers = %d/%d/%d/%v", report.Answers, report.UngradedAnswers, report.AnswerHits, report.AnswerRate)
	}
}

type alternatingJudge struct {
	calls int
}

func (judge *alternatingJudge) Judge(context.Context, Question, string) (bool, error) {
	judge.calls++
	if judge.calls == 1 {
		return true, nil
	}
	return false, errors.New("unrecognized judge verdict")
}
