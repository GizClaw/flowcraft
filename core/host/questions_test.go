package host

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

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
	answer, ok := route.Take(context.Background())
	if !ok || answer.Text() != "14A" {
		t.Fatalf("Take = %q, %v", answer.Text(), ok)
	}
	if _, ok := route.Take(context.Background()); ok {
		t.Fatal("the answer was taken twice")
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

// TestAnExpiredQuestionIsAskedAgain keeps a stale question from being answered minutes later.
func TestAnExpiredQuestionIsAskedAgain(t *testing.T) {
	route := newQuestionsRoute("s1", nil)
	route.Ask(context.Background(), message.NewTextContent("which seat?"))
	route.mu.Lock()
	route.posed.asked = time.Now().Add(-questionTTL - time.Second)
	route.mu.Unlock()
	if err := route.reply(message.NewTextContent("14A")); !errdefs.IsValidation(err) {
		t.Fatalf("reply = %v, want Validation for an expired question", err)
	}
	if again := route.Ask(context.Background(), message.NewTextContent("which seat?")); again == nil {
		t.Fatal("an expired question was not asked again")
	}
}

// TestEverySessionCarriesTheQuestionSurface pins the wiring: the slot the ask_user tool reads is on
// every session, delegated or not, because the tool's registration depends on it.
func TestEverySessionCarriesTheQuestionSurface(t *testing.T) {
	agents := deliveryHost(t, &deliveryDriver{accepts: true})
	session, ok := agents.Session("s1")
	if !ok {
		t.Fatal("the session is not open")
	}
	if _, ok := platform.AskSlot.Get(session.Binding().Caps); !ok {
		t.Fatal("the session carries no question surface")
	}
}
