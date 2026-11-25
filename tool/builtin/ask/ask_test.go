package ask

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// route is a question surface a test drives by hand.
type route struct {
	answer   *message.Content
	posed    int
	consumed int
}

func (r *route) Ask(_ context.Context, question message.Content) *message.Content {
	if r.answer != nil {
		return nil
	}
	r.posed++
	text := message.NewTextContent(question.Text() + " (waiting)")
	return &text
}

func (r *route) Take(context.Context, message.Content) (message.Content, bool) {
	r.consumed++
	if r.answer == nil {
		return message.Content{}, false
	}
	answer := *r.answer
	r.answer = nil
	return answer, true
}

func bindingFor(questions platform.Questions) tool.Binding {
	set := tool.NewCapSet()
	if questions != nil {
		platform.AskSlot.Provide(set, questions)
	}
	return tool.Bind(testSession{}, policy.Grant{}).WithCaps(set)
}

type testSession struct{}

func (testSession) ID() string            { return "ask-test" }
func (testSession) Scratch() tool.Scratch { return testScratch{} }

type testScratch struct{}

func (testScratch) Load(string) (any, bool) { return nil, false }
func (testScratch) Store(string, any)       {}
func (testScratch) Delete(string)           {}

func args(question string) string {
	if question == "" {
		return `{}`
	}
	return `{"question":"` + question + `"}`
}

// TestThePreflightPosesTheQuestion is what the tool node suspends on.
func TestThePreflightPosesTheQuestion(t *testing.T) {
	questions := &route{}
	question, err := (askTool{}).Ask(context.Background(), bindingFor(questions), args("which seat?"))
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if question == nil || question.Text() != "which seat? (waiting)" {
		t.Fatalf("question = %v", question)
	}
	if questions.posed != 1 {
		t.Fatalf("posed %d times", questions.posed)
	}
}

// TestAnAnsweredQuestionRunsWithoutAskingAgain: once the answer is there, the pre-flight is silent
// and Execute returns it as the call's result.
func TestAnAnsweredQuestionRunsWithoutAskingAgain(t *testing.T) {
	answer := message.NewTextContent("14A")
	questions := &route{answer: &answer}
	if question, err := (askTool{}).Ask(context.Background(), bindingFor(questions), args("which seat?")); err != nil || question != nil {
		t.Fatalf("Ask = %v, %v; want no question", question, err)
	}
	content, err := (askTool{}).Execute(context.Background(), bindingFor(questions), args("which seat?"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if content.Text() != "14A" {
		t.Fatalf("answer = %q", content.Text())
	}
}

// TestNoOutletIsAnError is the deployment's answer, not a suspended turn: the model sees it in the
// turn that asked, and can plan around it.
func TestNoOutletIsAnError(t *testing.T) {
	_, err := (askTool{}).Ask(context.Background(), bindingFor(nil), args("which seat?"))
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Ask = %v, want NotAvailable", err)
	}
}

// TestAQuestionIsRequired keeps an empty ask from suspending a turn nobody can answer: the
// pre-flight stays silent and Execute reports the mistake as a tool result the model can correct.
func TestAQuestionIsRequired(t *testing.T) {
	questions := &route{}
	question, err := (askTool{}).Ask(context.Background(), bindingFor(questions), args(""))
	if err != nil || question != nil {
		t.Fatalf("Ask = %v, %v; want no question and no run failure", question, err)
	}
	if _, err := (askTool{}).Execute(context.Background(), bindingFor(questions), args("")); !errdefs.IsValidation(err) {
		t.Fatalf("Execute = %v, want Validation", err)
	}
}
