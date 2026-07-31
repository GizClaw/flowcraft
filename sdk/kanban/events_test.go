package kanban_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

func subscribe(t *testing.T, k *kanban.Kanban, p event.Pattern) event.Subscription {
	t.Helper()
	sub, err := k.Bus().Subscribe(context.Background(), p, event.WithBufferSize(256))
	if err != nil {
		t.Fatalf("Subscribe(%q): %v", p, err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

func nextEvent(t *testing.T, sub event.Subscription) (event.Envelope, kanban.CardEvent) {
	t.Helper()
	select {
	case env, ok := <-sub.C():
		if !ok {
			t.Fatal("subscription closed early")
		}
		var payload kanban.CardEvent
		raw, err := json.Marshal(env.Payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("unmarshal CardEvent: %v", err)
		}
		return env, payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return event.Envelope{}, kanban.CardEvent{}
	}
}

func TestEventPerTransition(t *testing.T) {
	k := newBoard(t)
	sub := subscribe(t, k, kanban.PatternAll())

	ctx := kanban.WithProducerID(context.Background(), "planner")
	card, err := k.Submit(ctx, kanban.Task{
		TargetAgentID: "worker",
		Query:         "summarise",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	k.Claim(card.ID, "worker-1")
	k.Done(card.ID, kanban.Result{Output: "digest"})

	for _, want := range []struct {
		kind   string
		status kanban.Status
	}{
		{kanban.EventCardSubmitted, kanban.StatusPending},
		{kanban.EventCardClaimed, kanban.StatusClaimed},
		{kanban.EventCardDone, kanban.StatusDone},
	} {
		env, payload := nextEvent(t, sub)
		if got := env.Header(kanban.HeaderKind); got != want.kind {
			t.Errorf("kind header = %q, want %q", got, want.kind)
		}
		if got := env.Header(kanban.HeaderCardID); got != card.ID {
			t.Errorf("card_id header = %q, want %q", got, card.ID)
		}
		if env.KanbanScopeID() != "test-scope" {
			t.Errorf("scope header = %q", env.KanbanScopeID())
		}
		if payload.Status != want.status {
			t.Errorf("payload Status = %q, want %q", payload.Status, want.status)
		}
		if payload.Version != kanban.PayloadVersion {
			t.Errorf("Version = %d, want %d", payload.Version, kanban.PayloadVersion)
		}
		if payload.CardID != card.ID || payload.ScopeID != "test-scope" {
			t.Errorf("payload identity = %+v", payload)
		}
		if payload.TargetAgentID != "worker" || payload.Query != "summarise" {
			t.Errorf("payload task fields = %+v", payload)
		}
		if payload.Producer != "planner" {
			t.Errorf("Producer = %q", payload.Producer)
		}
	}
}

func TestEventCarriesTerminalResult(t *testing.T) {
	k := newBoard(t)
	sub := subscribe(t, k, kanban.PatternAll())
	card := mustSubmit(t, k, "w")
	k.Claim(card.ID, "worker")
	k.Fail(card.ID, "upstream exploded")

	nextEvent(t, sub) // submitted
	nextEvent(t, sub) // claimed
	env, payload := nextEvent(t, sub)

	if env.Header(kanban.HeaderKind) != kanban.EventCardFailed {
		t.Errorf("kind = %q", env.Header(kanban.HeaderKind))
	}
	if payload.Error != "upstream exploded" {
		t.Errorf("Error = %q", payload.Error)
	}
	if payload.Output != "" {
		t.Errorf("Output = %q, want empty on failure", payload.Output)
	}
}

func TestSuspendEventCarriesResumeRef(t *testing.T) {
	k := newBoard(t)
	sub := subscribe(t, k, kanban.PatternAll())
	card := mustSubmit(t, k, "w")
	k.Claim(card.ID, "worker")
	k.Suspend(card.ID, "checkpoint-7")

	nextEvent(t, sub)
	nextEvent(t, sub)
	env, payload := nextEvent(t, sub)

	if env.Header(kanban.HeaderKind) != kanban.EventCardSuspended {
		t.Errorf("kind = %q, want suspended", env.Header(kanban.HeaderKind))
	}
	if payload.ResumeRef != "checkpoint-7" {
		t.Errorf("ResumeRef = %q", payload.ResumeRef)
	}
	if payload.Status != kanban.StatusSuspended {
		t.Errorf("Status = %q", payload.Status)
	}
}

func TestPatternCardIsolatesOneCard(t *testing.T) {
	k := newBoard(t)
	mine := mustSubmit(t, k, "w")
	sub := subscribe(t, k, kanban.PatternCard(mine.ID))

	other := mustSubmit(t, k, "w")
	k.Claim(other.ID, "worker")
	k.Claim(mine.ID, "worker")

	_, payload := nextEvent(t, sub)
	if payload.CardID != mine.ID {
		t.Fatalf("received an event for %q, want only %q", payload.CardID, mine.ID)
	}
	if payload.Status != kanban.StatusClaimed {
		t.Errorf("Status = %q", payload.Status)
	}
}

func TestSubjectShapeAndSanitisation(t *testing.T) {
	k := newBoard(t)
	sub := subscribe(t, k, kanban.PatternAll())
	card := mustSubmit(t, k, "w")

	env, _ := nextEvent(t, sub)
	want := "kanban.card." + card.ID + ".submitted"
	if string(env.Subject) != want {
		t.Errorf("Subject = %q, want %q", env.Subject, want)
	}

	// A card id can never contain a separator, but the pattern helpers
	// take caller-supplied ids and must not let one forge a wildcard
	// segment or split the subject.
	got := kanban.PatternCard("a.b*c>d")
	if want := event.Pattern("kanban.card.a_b_c_d.>"); got != want {
		t.Errorf("PatternCard = %q, want %q", got, want)
	}
}

func TestEventsStopAfterClose(t *testing.T) {
	k := kanban.New("scope")
	sub, err := k.Bus().Subscribe(context.Background(), kanban.PatternAll())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := k.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Error("received an event after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscription channel not closed by board Close")
	}
}
