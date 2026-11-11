package eval

import "testing"

func TestCompareAnswersReportsRegressions(t *testing.T) {
	baseline := NewBaseline("baseline", []Report{
		{Scenario: "conv-1", AnswerRate: 0.80, Answers: 10},
		{Scenario: "recall-only", HitRate: 0.50},
	})
	current := []Report{
		{Scenario: "conv-1", AnswerRate: 0.75, Answers: 10},
		{Scenario: "recall-only", HitRate: 0.50},
	}
	if regressions := baseline.CompareAnswers(current, 0.02); len(regressions) != 1 ||
		regressions[0].Metric != "answer_rate" || regressions[0].Scenario != "conv-1" {
		t.Fatalf("regressions = %#v", regressions)
	}
	if regressions := baseline.CompareAnswers(current, 0.06); len(regressions) != 0 {
		t.Fatalf("tolerance ignored: %#v", regressions)
	}
	if regressions := baseline.Compare(current, 0.02); len(regressions) != 0 {
		t.Fatalf("unchanged recall flagged: %#v", regressions)
	}
}
