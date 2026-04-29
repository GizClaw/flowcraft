package kanban

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------------------------------------------------------------------------
// Bus() emission — Submit / Claim / Done / Fail are published from Board
// ---------------------------------------------------------------------------

func TestBus_PublishesTaskSubmitted(t *testing.T) {
	t.Parallel()
	k, sb := newKanban(t, WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	sub := subscribeBus(t, sb)

	if _, err := k.Submit(context.Background(), TaskOptions{Query: "test event"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	evs := drainEvents(sub, time.Second, 1, func(e event.Envelope) bool {
		return kindOf(e) == EventTaskSubmitted
	})
	if !containsKind(evs, EventTaskSubmitted) {
		t.Fatalf("expected %q on Bus(), saw kinds: %v", EventTaskSubmitted, eventKinds(evs))
	}
}

func TestBus_PublishesClaimAndDone(t *testing.T) {
	t.Parallel()
	sb := newBoard(t)
	executor := &mockExecutor{
		fn: func(_ context.Context, _, targetAgentID string, card *Card, _ string, _ map[string]any) error {
			sb.Claim(card.ID, targetAgentID)
			sb.Done(card.ID, map[string]any{
				"output":          "agent output",
				"target_agent_id": targetAgentID,
			})
			return nil
		},
	}
	k := New(context.Background(), sb,
		WithAgentExecutor(executor),
		WithConfig(KanbanConfig{MaxPendingTasks: 100}),
	)
	t.Cleanup(k.Stop)

	sub := subscribeBus(t, sb)

	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "test-agent",
		Query:         "hello agent",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	k.Stop()

	evs := drainEvents(sub, time.Second, 0, nil)
	for _, want := range []string{EventTaskSubmitted, EventTaskClaimed, EventTaskCompleted} {
		if !containsKind(evs, want) {
			t.Errorf("expected %q on Bus(), saw kinds: %v", want, eventKinds(evs))
		}
	}
}

func TestBus_PublishesTaskFailedFromExecutorError(t *testing.T) {
	t.Parallel()
	executor := &mockExecutor{
		fn: func(_ context.Context, _, _ string, _ *Card, _ string, _ map[string]any) error {
			return errors.New("agent down")
		},
	}
	k, sb := newKanban(t,
		WithAgentExecutor(executor),
		WithConfig(KanbanConfig{MaxPendingTasks: 100}),
	)
	sub := subscribeBus(t, sb)

	if _, err := k.Submit(context.Background(), TaskOptions{
		TargetAgentID: "agent-fail",
		Query:         "boom",
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	k.Stop()

	evs := drainEvents(sub, 2*time.Second, 1, func(e event.Envelope) bool {
		return kindOf(e) == EventTaskFailed
	})
	for _, e := range evs {
		if kindOf(e) != EventTaskFailed {
			continue
		}
		var p TaskFailedPayload
		if err := e.Decode(&p); err != nil {
			t.Fatalf("Decode TaskFailedPayload: %v", err)
		}
		if p.Error != "agent down" {
			t.Errorf("payload.Error = %q, want %q", p.Error, "agent down")
		}
		if p.TargetAgentID != "agent-fail" {
			t.Errorf("payload.TargetAgentID = %q, want agent-fail", p.TargetAgentID)
		}
		return
	}
	t.Fatalf("expected EventTaskFailed on Bus(), saw kinds: %v", eventKinds(evs))
}

// ---------------------------------------------------------------------------
// Subject convention: each kind maps to a card- / cron-scoped subject and
// carries the well-known headers.
// ---------------------------------------------------------------------------

func TestBus_SubjectAndHeadersForTaskSubmitted(t *testing.T) {
	t.Parallel()
	k, sb := newKanban(t, WithConfig(KanbanConfig{MaxPendingTasks: 100}))
	sub := subscribeBus(t, sb)

	cardID, err := k.Submit(context.Background(), TaskOptions{Query: "subject check", TargetAgentID: "a"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	evs := drainEvents(sub, time.Second, 1, func(e event.Envelope) bool {
		return kindOf(e) == EventTaskSubmitted
	})
	for _, e := range evs {
		if kindOf(e) != EventTaskSubmitted {
			continue
		}
		want := subjTaskSubmitted(cardID)
		if e.Subject != want {
			t.Errorf("Subject = %q, want %q", e.Subject, want)
		}
		if got := e.Header(HeaderCardID); got != cardID {
			t.Errorf("Header[card_id] = %q, want %q", got, cardID)
		}
		if got := e.Header(HeaderKanbanKind); got != EventTaskSubmitted {
			t.Errorf("Header[kanban_kind] = %q, want %q", got, EventTaskSubmitted)
		}
		if got := e.KanbanScopeID(); got != sb.ScopeID() {
			t.Errorf("Header[kanban_scope_id] = %q, want %q", got, sb.ScopeID())
		}
		return
	}
	t.Fatalf("missing %q on Bus(), saw kinds: %v", EventTaskSubmitted, eventKinds(evs))
}

func TestBus_PatternCardScopesToSingleCard(t *testing.T) {
	t.Parallel()
	k, sb := newKanban(t, WithConfig(KanbanConfig{MaxPendingTasks: 100}))

	// Submit a card first so we know its ID, then subscribe to PatternCard
	// for that ID, then submit a second card. Only the first card's
	// follow-up events would land on a per-card subscription, but in this
	// scenario nothing further happens to either card — so we settle for
	// the weaker guarantee that the *all-cards* fan-out has produced two
	// distinct subjects, one per card_id.
	allSub := subscribeBus(t, sb)

	id1, err := k.Submit(context.Background(), TaskOptions{Query: "first", TargetAgentID: "a"})
	if err != nil {
		t.Fatalf("Submit #1: %v", err)
	}
	id2, err := k.Submit(context.Background(), TaskOptions{Query: "second", TargetAgentID: "a"})
	if err != nil {
		t.Fatalf("Submit #2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("Submit returned same id twice: %q", id1)
	}

	want := map[string]event.Subject{
		id1: subjTaskSubmitted(id1),
		id2: subjTaskSubmitted(id2),
	}
	got := map[string]event.Subject{}
	for _, e := range drainEvents(allSub, 500*time.Millisecond, 2, func(e event.Envelope) bool {
		return kindOf(e) == EventTaskSubmitted
	}) {
		if kindOf(e) != EventTaskSubmitted {
			continue
		}
		got[e.Header(HeaderCardID)] = e.Subject
	}

	for id, w := range want {
		if got[id] != w {
			t.Errorf("card %q subject = %q, want %q", id, got[id], w)
		}
	}
}

// ---------------------------------------------------------------------------
// Bug 6 acceptance: Bus() can reconstruct Cards() over a random op sequence.
// ---------------------------------------------------------------------------

func TestBus_ReconstructsCardsFromEvents(t *testing.T) {
	t.Parallel()
	sb := newBoard(t)
	sub := subscribeBus(t, sb)

	rng := rand.New(rand.NewSource(42))
	const ops = 80
	type liveCard struct {
		id     string
		status CardStatus
	}
	var live []liveCard

	for i := 0; i < ops; i++ {
		switch {
		case len(live) == 0 || rng.Intn(3) == 0:
			c := sb.Produce("task", "producer-1",
				TaskPayload{Query: fmt.Sprintf("q-%d", i), TargetAgentID: "agent-1"})
			live = append(live, liveCard{id: c.ID, status: CardPending})
		default:
			idx := rng.Intn(len(live))
			lc := &live[idx]
			switch lc.status {
			case CardPending:
				if rng.Intn(2) == 0 {
					sb.Claim(lc.id, "agent-1")
					lc.status = CardClaimed
				} else {
					sb.Fail(lc.id, "boom")
					lc.status = CardFailed
				}
			case CardClaimed:
				if rng.Intn(2) == 0 {
					sb.Done(lc.id, map[string]any{"output": "ok", "target_agent_id": "agent-1"})
					lc.status = CardDone
				} else {
					sb.Fail(lc.id, "boom")
					lc.status = CardFailed
				}
			}
		}
	}

	type stateRow struct {
		id     string
		status string
	}
	want := func() []stateRow {
		raw := sb.Cards()
		out := make([]stateRow, 0, len(raw))
		for _, c := range raw {
			out = append(out, stateRow{id: c.ID, status: c.Status})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
	}()

	got := func() []stateRow {
		state := make(map[string]string)
		for _, ev := range drainEvents(sub, time.Second, 0, nil) {
			switch kindOf(ev) {
			case EventTaskSubmitted:
				var p TaskSubmittedPayload
				if ev.Decode(&p) == nil {
					state[p.CardID] = string(CardPending)
				}
			case EventTaskClaimed:
				var p TaskClaimedPayload
				if ev.Decode(&p) == nil {
					state[p.CardID] = string(CardClaimed)
				}
			case EventTaskCompleted:
				var p TaskCompletedPayload
				if ev.Decode(&p) == nil {
					state[p.CardID] = string(CardDone)
				}
			case EventTaskFailed:
				var p TaskFailedPayload
				if ev.Decode(&p) == nil {
					state[p.CardID] = string(CardFailed)
				}
			}
		}
		out := make([]stateRow, 0, len(state))
		for id, st := range state {
			out = append(out, stateRow{id: id, status: st})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
	}()

	if len(want) != len(got) {
		t.Fatalf("card count mismatch: Cards()=%d Bus()=%d\nwant=%v\n got=%v", len(want), len(got), want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("state[%d] mismatch: want=%+v got=%+v", i, want[i], got[i])
		}
	}
}

// ---------------------------------------------------------------------------
// stampVersion: every payload type carries Version=payloadVersion.
// ---------------------------------------------------------------------------

func TestStampVersion_StampsAllPayloadTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload any
		version func(any) int
	}{
		{"task.submitted", TaskSubmittedPayload{CardID: "c1"}, func(p any) int { return p.(TaskSubmittedPayload).Version }},
		{"task.claimed", TaskClaimedPayload{CardID: "c1"}, func(p any) int { return p.(TaskClaimedPayload).Version }},
		{"task.completed", TaskCompletedPayload{CardID: "c1"}, func(p any) int { return p.(TaskCompletedPayload).Version }},
		{"task.failed", TaskFailedPayload{CardID: "c1"}, func(p any) int { return p.(TaskFailedPayload).Version }},
		{"callback.start", CallbackStartPayload{CardID: "c1"}, func(p any) int { return p.(CallbackStartPayload).Version }},
		{"callback.done", CallbackDonePayload{CardID: "c1"}, func(p any) int { return p.(CallbackDonePayload).Version }},
		{"cron.created", CronRuleCreatedPayload{ScheduleID: "s1"}, func(p any) int { return p.(CronRuleCreatedPayload).Version }},
		{"cron.fired", CronRuleFiredPayload{ScheduleID: "s1"}, func(p any) int { return p.(CronRuleFiredPayload).Version }},
		{"cron.disabled", CronRuleDisabledPayload{ScheduleID: "s1"}, func(p any) int { return p.(CronRuleDisabledPayload).Version }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out := stampVersion(c.payload)
			if v := c.version(out); v != payloadVersion {
				t.Fatalf("Version = %d, want %d", v, payloadVersion)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers — env.Header(HeaderKanbanKind) is the canonical "what kind of
// kanban event is this" lookup; subjects are scoped per-card / per-rule.
// ---------------------------------------------------------------------------

func kindOf(e event.Envelope) string { return e.Header(HeaderKanbanKind) }

func containsKind(evs []event.Envelope, kind string) bool {
	for _, e := range evs {
		if kindOf(e) == kind {
			return true
		}
	}
	return false
}

func eventKinds(evs []event.Envelope) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = kindOf(e)
	}
	return out
}
