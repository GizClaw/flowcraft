package answer

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

type fakeRuntime struct {
	requests []inference.GenerateRequest
	reply    string
}

func (runtime *fakeRuntime) Generate(_ context.Context, _ model.ModelRef, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	runtime.requests = append(runtime.requests, request)
	return inference.GenerateResponse{
		Message: coremessage.NewTextMessage(coremessage.RoleAssistant, runtime.reply),
	}, nil
}

// TestAnswerPolicyKeepsItsLoadBearingRules guards the answer policy: every rule
// below was added to fix a measured failure mode (hedging on open-domain
// questions, dropping concrete list items, inventing facts about the people),
// so removing one should be a deliberate, visible change.
func TestAnswerPolicyKeepsItsLoadBearingRules(t *testing.T) {
	for _, rule := range []string{
		"Ground every claim",
		"general world knowledge",
		"mark it as likely",
		"list every item",
		"exactly as stated",
		"nothing relevant",
	} {
		if !strings.Contains(answerSystem, rule) {
			t.Fatalf("answer policy no longer contains %q", rule)
		}
	}
	if AnswerPromptVersion == "" || JudgePromptVersion == "" {
		t.Fatal("prompt versions must be set: they pin the run fingerprint")
	}
}

func TestModelAnswerRendersContextAndQuestion(t *testing.T) {
	runtime := &fakeRuntime{reply: "  7 May 2023  "}
	model, err := New(runtime, model.ModelRef{ID: model.ModelID{Provider: "deepseek", Name: "deepseek-chat"}})
	if err != nil {
		t.Fatal(err)
	}
	items := []corememory.ContextItem{{
		ID: "item-1", Kind: corememory.ContextRawMessage, SourceClass: corememory.ContextSourceLongTerm,
		Content: coremessage.NewTextContent("[8 May 2023] Caroline: I went to a support group yesterday."),
		Sources: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "m1"}},
	}}
	got, err := model.Answer(context.Background(), eval.Question{Query: "When did Caroline go to the support group?"}, items)
	if err != nil {
		t.Fatal(err)
	}
	if got != "7 May 2023" {
		t.Fatalf("answer = %q", got)
	}
	prompt := runtime.requests[0].Input.Content.Text()
	if !strings.Contains(prompt, "support group yesterday") || !strings.Contains(prompt, "When did Caroline") {
		t.Fatalf("prompt does not carry context and question: %q", prompt)
	}
}

func TestModelJudgeParsesVerdicts(t *testing.T) {
	for _, test := range []struct {
		reply string
		want  bool
	}{
		{"CORRECT", true},
		{"incorrect", false},
		{" Correct. ", true},
		{`{"correct": true}`, true},
		{`{"correct": false}`, false},
		{`{"correct": false, "reason": "the date differs"}`, false},
	} {
		runtime := &fakeRuntime{reply: test.reply}
		model, err := New(runtime, model.ModelRef{ID: model.ModelID{Provider: "deepseek", Name: "deepseek-chat"}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := model.Judge(context.Background(), eval.Question{Query: "q", WantContains: []string{"a"}}, "candidate")
		if err != nil {
			t.Fatalf("verdict %q: %v", test.reply, err)
		}
		if got != test.want {
			t.Fatalf("verdict %q = %v, want %v", test.reply, got, test.want)
		}
	}
	if model, err := New(&fakeRuntime{}, model.ModelRef{}); err == nil || model != nil {
		t.Fatal("empty model ref accepted")
	}
}

// TestModelJudgeRejectsAmbiguousVerdicts is the regression guard for the old
// substring scan: prose that hedges or negates used to be read as CORRECT
// ("not fully correct" contains "correct"), which inflated the answer rate.
func TestModelJudgeRejectsAmbiguousVerdicts(t *testing.T) {
	for _, reply := range []string{
		"The prediction is not fully correct.",
		"The answer is partially correct but misses the date.",
		"It is correct that she moved, but the city differs.",
		"The gold answer refers to a date (September 2023), while the generated answer discusses a talent show and doesn't mention any date or time period matching the gold answer.",
		`{"verdict": "correct"}`,
		"",
	} {
		model, err := New(&fakeRuntime{reply: reply}, model.ModelRef{ID: model.ModelID{Provider: "deepseek", Name: "deepseek-chat"}})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := model.Judge(context.Background(), eval.Question{Query: "q"}, "candidate"); err == nil {
			t.Fatalf("verdict %q was accepted as %v", reply, got)
		}
	}
}
