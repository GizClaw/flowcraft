package host

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// TestASessionWithoutAnOutletHasNoQuestionSurface is the deployment's answer: a root session whose
// application installed no outlet cannot ask, so the tool refuses the call instead of suspending a
// turn nobody was told about.
func TestASessionWithoutAnOutletHasNoQuestionSurface(t *testing.T) {
	agents := deliveryHost(t, &deliveryDriver{accepts: true})
	session, ok := agents.Session("s1")
	if !ok {
		t.Fatal("the session is not open")
	}
	if _, ok := platform.AskSlot.Get(session.Binding().Caps); ok {
		t.Fatal("a session with no outlet carries a question surface")
	}
}

// TestAQuestionIsPosedAndItsAnswerTaken is the tool's round trip: Ask reports the text to suspend
// on, the answer arrives with the resume, and Take hands it to the call.
func TestAQuestionIsPosedAndItsAnswerTaken(t *testing.T) {
	var asked []string
	route := newQuestionsRoute("s1", func(_ context.Context, _ string, q message.Content) bool {
		asked = append(asked, q.Text())
		return true
	})
	question := message.NewTextContent("which seat?")
	got := route.Ask(context.Background(), question)
	if got == nil || got.Text() != "which seat?" {
		t.Fatalf("Ask = %v, want the question text", got)
	}
	if len(asked) != 1 {
		t.Fatalf("the outlet was told %d times, want once", len(asked))
	}
	// Asking again while the question is open returns the same text without asking the outlet twice.
	if again := route.Ask(context.Background(), question); again == nil || again.Text() != "which seat?" {
		t.Fatalf("Ask again = %v", again)
	}
	if len(asked) != 1 {
		t.Fatalf("the outlet was told %d times for one question", len(asked))
	}
	if err := route.reply(message.NewTextContent("14A")); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if answered := route.Ask(context.Background(), question); answered != nil {
		t.Fatalf("Ask after the answer = %q, want none", answered.Text())
	}
	answer, ok := route.Take(context.Background(), question)
	if !ok || answer.Text() != "14A" {
		t.Fatalf("Take = %q, %v", answer.Text(), ok)
	}
	if _, ok := route.Take(context.Background(), question); ok {
		t.Fatal("the answer was taken twice")
	}
}

// TestAnAnswerReachesOnlyItsOwnQuestion is the binding: a reply is keyed by the question's text, so
// a later, different question is not answered by it.
func TestAnAnswerReachesOnlyItsOwnQuestion(t *testing.T) {
	route := newQuestionsRoute("s1", nil)
	first := message.NewTextContent("which seat?")
	route.Ask(context.Background(), first)
	if err := route.reply(message.NewTextContent("14A")); err != nil {
		t.Fatalf("reply: %v", err)
	}
	second := message.NewTextContent("which meal?")
	if question := route.Ask(context.Background(), second); question == nil {
		t.Fatal("the second question was answered by the first question's reply")
	}
	if _, ok := route.Take(context.Background(), second); ok {
		t.Fatal("the first answer was served to the second question")
	}
	if answer, ok := route.Take(context.Background(), first); !ok || answer.Text() != "14A" {
		t.Fatalf("Take(first) = %q, %v", answer.Text(), ok)
	}
}

// TestAnExpiredAnswerDoesNotWedgeTheSession: the reply is dropped, and what the caller does next is
// resume — the pre-flight asks again rather than the session staying paused with an answer nobody
// will take.
func TestAnExpiredAnswerDoesNotWedgeTheSession(t *testing.T) {
	route := newQuestionsRoute("s1", nil)
	question := message.NewTextContent("which seat?")
	route.Ask(context.Background(), question)
	route.mu.Lock()
	for _, posed := range route.posed {
		posed.asked = time.Now().Add(-questionTTL - time.Second)
	}
	route.mu.Unlock()
	if err := route.reply(message.NewTextContent("14A")); err != nil {
		t.Fatalf("reply on an expired question = %v, want it dropped", err)
	}
	if again := route.Ask(context.Background(), question); again == nil {
		t.Fatal("the question was not asked again after the reply expired")
	}
}

// TestAnAnswerWithoutAQuestionIsRefused keeps the two turns apart: an answer to nothing is a caller
// mistake, not a message to store.
func TestAnAnswerWithoutAQuestionIsRefused(t *testing.T) {
	route := newQuestionsRoute("s1", nil)
	if err := route.reply(message.NewTextContent("14A")); !errdefs.IsValidation(err) {
		t.Fatalf("reply = %v, want Validation", err)
	}
}

// TestASessionWithAnOutletCarriesTheQuestionSurface pins the wiring: the slot the ask_user tool
// reads is on a session whose application installed an outlet.
func TestASessionWithAnOutletCarriesTheQuestionSurface(t *testing.T) {
	agents := deliveryHost(t, &deliveryDriver{accepts: true},
		WithQuestions(func(context.Context, string, message.Content) bool { return true }))
	session, ok := agents.Session("s1")
	if !ok {
		t.Fatal("the session is not open")
	}
	if _, ok := platform.AskSlot.Get(session.Binding().Caps); !ok {
		t.Fatal("the session carries no question surface")
	}
}
