package answer

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
)

// The LongMemEval protocol contains three kinds of question, and one rubric
// cannot grade all of them: a fact row is graded on the gold answer, a
// preference row on the preferences it states, and an "_abs" row on the
// refusal. The marker in each rubric below is a rule the other two do not
// carry, so a dispatch that picks the wrong one fails here.
func TestJudgeLongMemEvalDispatchesByQuestionLabel(t *testing.T) {
	for _, test := range []struct {
		name     string
		question eval.Question
		marker   string
		absent   string
	}{
		{
			name:     "fact",
			question: eval.Question{Query: "q", Category: eval.LongMemEvalMultiSession, CategoryLabel: "multi-session"},
			marker:   "subset of the information the gold requires",
			absent:   "what this user prefers",
		},
		{
			name:     "preference",
			question: eval.Question{Query: "q", Category: eval.LongMemEvalSingleSessionPreference, CategoryLabel: "single-session-preference"},
			marker:   "what this user prefers",
			absent:   "does not contain what the question asks",
		},
		{
			// The abstention marker wins over the preference bucket: a row the
			// history cannot answer is graded on the refusal whatever type it
			// is filed under.
			name: "abstention",
			question: eval.Question{
				Query:        "q",
				Category:     eval.LongMemEvalSingleSessionPreference,
				Unanswerable: true,
			},
			marker: "does not contain what the question asks",
			absent: "what this user prefers",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeRuntime{reply: "CORRECT"}
			model := newAnswerModel(t, runtime).WithJudgeStyle(JudgeLongMemEval)
			correct, err := model.Judge(context.Background(), test.question, "a prediction")
			if err != nil {
				t.Fatal(err)
			}
			if !correct {
				t.Fatal("the judge did not read the verdict it was given")
			}
			if len(runtime.requests) != 1 {
				t.Fatalf("generate calls = %d", len(runtime.requests))
			}
			system := runtime.requests[0].Context[0].Content.Text()
			if !strings.Contains(system, test.marker) {
				t.Fatalf("%s rubric missing %q:\n%s", test.name, test.marker, system)
			}
			if strings.Contains(system, test.absent) {
				t.Fatalf("%s rubric carries another type's rule %q:\n%s", test.name, test.absent, system)
			}
			// A judge without the gold graders nothing: unlike the answering
			// stage, whose prompt must not carry dataset labels, this one has to.
			if !strings.Contains(runtime.requests[0].Input.Content.Text(), "Gold:") {
				t.Fatalf("%s judge prompt lost the gold:\n%s", test.name, runtime.requests[0].Input.Content.Text())
			}
		})
	}
}

// The fingerprint records which grading protocol produced a report, and the two
// protocols do not grade the same gold shape. LoCoMo's styles keep the historical
// version string so reports measured before LongMemEval support still compare.
func TestPromptVersionNamesTheProtocol(t *testing.T) {
	if got := PromptVersion(JudgeLongMemEval); got != JudgeLongMemEvalPromptVersion {
		t.Fatalf("lme prompt version = %q", got)
	}
	for _, style := range []JudgeStyle{JudgeStrict, JudgeLocoMo, "both"} {
		if got := PromptVersion(style); got != JudgePromptVersion {
			t.Fatalf("%q prompt version = %q, want %q", style, got, JudgePromptVersion)
		}
	}
}

// An unknown style must not silently change how a run is graded: the model
// keeps the strict default.
func TestWithJudgeStyleRejectsUnknownStyles(t *testing.T) {
	model := newAnswerModel(t, &fakeRuntime{reply: "CORRECT"})
	if model.WithJudgeStyle("nonsense").judgeStyle != JudgeStrict {
		t.Fatal("an unknown judge style replaced the default")
	}
	if model.WithJudgeStyle(JudgeLongMemEval).judgeStyle != JudgeLongMemEval {
		t.Fatal("the longmemeval style was not applied")
	}
}

// The asked-at date is what a relative-time question resolves against, and the
// loader fills it from the dataset: the answering prompt is where it has to
// arrive.
func TestAnswerPromptCarriesTheQuestionDate(t *testing.T) {
	runtime := &fakeRuntime{reply: "two months"}
	model := newAnswerModel(t, runtime)
	question := eval.Question{
		Query:         "How long after I adopted the dog did I move?",
		WantContains:  []string{"two months"},
		AskedAt:       "2023/07/02 (Sun) 09:15",
		Category:      eval.LongMemEvalTemporalReasoning,
		CategoryLabel: "temporal-reasoning",
	}
	if _, err := model.Answer(context.Background(), question, labelledItems()); err != nil {
		t.Fatal(err)
	}
	prompt := runtime.requests[0].Input.Content.Text()
	if !strings.Contains(prompt, "The question was asked at: 2023/07/02 (Sun) 09:15") {
		t.Fatalf("answer prompt lost the question date:\n%s", prompt)
	}
	// The category is a grading label, not a prompt input: the temporal suffix
	// belongs to LoCoMo's category 2 and this question is not one.
	if strings.Contains(prompt, officialTemporalHint) {
		t.Fatalf("the LoCoMo temporal suffix leaked into a LongMemEval question:\n%s", prompt)
	}
}
