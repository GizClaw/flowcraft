package answer

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

func newAnswerModel(t *testing.T, runtime Runtime) *Model {
	t.Helper()
	built, err := New(runtime, model.ModelRef{ID: model.ModelID{Provider: "deepseek", Name: "deepseek-flash"}})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

// labelledQuestion carries every dataset-side label a question can hold: the
// gold answer, the evidence turn ids, and the LoCoMo category.
func labelledQuestion() eval.Question {
	return eval.Question{
		Query:        "When did Caroline go to the support group?",
		WantContains: []string{"7 May 2023"},
		Evidence:     []string{"D1:3"},
		Category:     eval.TemporalCategory,
	}
}

func labelledItems() []corememory.ContextItem {
	return []corememory.ContextItem{{
		ID: "item-1", Kind: corememory.ContextRawMessage, SourceClass: corememory.ContextSourceLongTerm,
		Content: coremessage.NewTextContent("[8 May 2023] Caroline: I went to a support group yesterday."),
		Sources: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "m1"}},
	}}
}

// TestAnswerPromptCarriesNoDatasetLabels pins the harness boundary: a question's
// gold answer, its evidence turn ids and its category label are grading
// metadata. The answer model gets the question and the recalled context and
// nothing else -- a gold string in the prompt would make every answered run an
// oracle, and a run graded against its own leaked gold is not a measurement.
func TestAnswerPromptCarriesNoDatasetLabels(t *testing.T) {
	runtime := &fakeRuntime{reply: "7 May 2023"}
	model := newAnswerModel(t, runtime)
	question := labelledQuestion()
	if _, err := model.Answer(context.Background(), question, labelledItems()); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("generate calls = %d, want 1", len(runtime.requests))
	}
	request := runtime.requests[0]
	system := request.Context[0].Content.Text()
	user := request.Input.Content.Text()
	for _, prompt := range []struct {
		name string
		text string
	}{{"answer system prompt", system}, {"answer user prompt", user}} {
		for _, leak := range []string{"7 May 2023", "D1:3", "ategory"} {
			if strings.Contains(prompt.text, leak) {
				t.Fatalf("%s carries the dataset label %q:\n%s", prompt.name, leak, prompt.text)
			}
		}
	}
	if !strings.Contains(user, question.Query) {
		t.Fatalf("answer prompt lost the question:\n%s", user)
	}
	// The suffix is the reference harness's, not ours: without the opt-in flag
	// the question stays exactly as the dataset wrote it.
	if strings.Contains(user, officialTemporalHint) {
		t.Fatalf("the reference harness's temporal suffix leaked into a default run:\n%s", user)
	}

	// The judge is the one stage that must see the gold: that is its job.
	runtime.reply = "CORRECT"
	if _, err := model.Judge(context.Background(), question, "7 May 2023"); err != nil {
		t.Fatal(err)
	}
	judgeUserPrompt := runtime.requests[1].Input.Content.Text()
	if !strings.Contains(judgeUserPrompt, "7 May 2023") || !strings.Contains(judgeUserPrompt, question.Query) {
		t.Fatalf("judge prompt must carry question and gold:\n%s", judgeUserPrompt)
	}
}

// TestTemporalHintMirrorsTheReferenceHarness pins the opt-in flag to the
// reference harness's behaviour: the suffix goes to category-2 questions only,
// verbatim, and reaches every answering style, because the official protocol
// appends it to the question before the prompt is built.
func TestTemporalHintMirrorsTheReferenceHarness(t *testing.T) {
	for _, style := range []AnswerStyle{AnswerStyleLong, AnswerStyleShort, AnswerStyleEvidence} {
		reply := "7 May 2023"
		if style == AnswerStyleEvidence {
			reply = "Quotes: went to a support group\nShort answer: 7 May 2023"
		}
		runtime := &fakeRuntime{reply: reply}
		model := newAnswerModel(t, runtime).WithAnswerStyle(style).WithTemporalHint(true)
		question := labelledQuestion()
		if _, err := model.Answer(context.Background(), question, labelledItems()); err != nil {
			t.Fatal(err)
		}
		user := runtime.requests[0].Input.Content.Text()
		suffix := question.Query + officialTemporalHint
		if !strings.Contains(user, suffix) {
			t.Fatalf("style %s: temporal question does not carry the reference suffix %q:\n%s", style, officialTemporalHint, user)
		}
		for _, category := range []int{1, 3, 4} {
			other := question
			other.Category = category
			if _, err := model.Answer(context.Background(), other, labelledItems()); err != nil {
				t.Fatal(err)
			}
			prompt := runtime.requests[len(runtime.requests)-1].Input.Content.Text()
			if strings.Contains(prompt, officialTemporalHint) {
				t.Fatalf("style %s category %d: the temporal suffix is category-2 only:\n%s", style, category, prompt)
			}
			if !strings.Contains(prompt, question.Query) {
				t.Fatalf("style %s category %d: question missing:\n%s", style, category, prompt)
			}
		}
	}
}

// TestWithTemporalHintIsOffByDefault guards the default: the product answering
// protocol must not route on a dataset category, so a model built without the
// flag never appends the suffix.
func TestWithTemporalHintIsOffByDefault(t *testing.T) {
	runtime := &fakeRuntime{reply: "7 May 2023"}
	model := newAnswerModel(t, runtime).WithTemporalHint(false)
	if _, err := model.Answer(context.Background(), labelledQuestion(), labelledItems()); err != nil {
		t.Fatal(err)
	}
	if user := runtime.requests[0].Input.Content.Text(); strings.Contains(user, officialTemporalHint) {
		t.Fatalf("temporal suffix present in a default run:\n%s", user)
	}
}
