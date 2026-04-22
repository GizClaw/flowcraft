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

	evs := drainEvents(sub, time.Second, 1, func(e event.Event) bool {
		return string(e.Type) == EventTaskSubmitted
	})
	if !containsType(evs, EventTaskSubmitted) {
		t.Fatalf("expected %q on Bus(), saw types: %v", EventTaskSubmitted, eventTypes(evs))
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
		if !containsType(evs, want) {
			t.Errorf("expected %q on Bus(), saw types: %v", want, eventTypes(evs))
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

	evs := drainEvents(sub, 2*time.Second, 1, func(e event.Event) bool {
		return string(e.Type) == EventTaskFailed
	})
	for _, e := range evs {
		if string(e.Type) != EventTaskFailed {
			continue
		}
		p, ok := e.Payload.(TaskFailedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want TaskFailedPayload", e.Payload)
		}
		if p.Error != "agent down" {
			t.Errorf("payload.Error = %q, want %q", p.Error, "agent down")
		}
		if p.TargetAgentID != "agent-fail" {
			t.Errorf("payload.TargetAgentID = %q, want agent-fail", p.TargetAgentID)
		}
		return
	}
	t.Fatalf("expected EventTaskFailed on Bus(), saw types: %v", eventTypes(evs))
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

	// Cards() = ground truth, sorted by ID.
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

	// Bus() = derived state by replaying event payloads.
	got := func() []stateRow {
		state := make(map[string]string)
		for _, ev := range drainEvents(sub, time.Second, 0, nil) {
			switch ev.Type {
			case EventTaskSubmitted:
				if p, ok := ev.Payload.(TaskSubmittedPayload); ok {
					state[p.CardID] = string(CardPending)
				}
			case EventTaskClaimed:
				if p, ok := ev.Payload.(TaskClaimedPayload); ok {
					state[p.CardID] = string(CardClaimed)
				}
			case EventTaskCompleted:
				if p, ok := ev.Payload.(TaskCompletedPayload); ok {
					state[p.CardID] = string(CardDone)
				}
			case EventTaskFailed:
				if p, ok := ev.Payload.(TaskFailedPayload); ok {
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
// Envelope: every payload carries Version=payloadVersion regardless of input.
// ---------------------------------------------------------------------------

func TestEventEnvelope_StampsVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload any
		version func(event.Event) int
	}{
		{"task.submitted", TaskSubmittedPayload{CardID: "c1"}, func(e event.Event) int { return e.Payload.(TaskSubmittedPayload).Version }},
		{"task.claimed", TaskClaimedPayload{CardID: "c1"}, func(e event.Event) int { return e.Payload.(TaskClaimedPayload).Version }},
		{"task.completed", TaskCompletedPayload{CardID: "c1"}, func(e event.Event) int { return e.Payload.(TaskCompletedPayload).Version }},
		{"task.failed", TaskFailedPayload{CardID: "c1"}, func(e event.Event) int { return e.Payload.(TaskFailedPayload).Version }},
		{"cron.created", CronRuleCreatedPayload{ScheduleID: "s1"}, func(e event.Event) int { return e.Payload.(CronRuleCreatedPayload).Version }},
		{"cron.fired", CronRuleFiredPayload{ScheduleID: "s1"}, func(e event.Event) int { return e.Payload.(CronRuleFiredPayload).Version }},
		{"cron.disabled", CronRuleDisabledPayload{ScheduleID: "s1"}, func(e event.Event) int { return e.Payload.(CronRuleDisabledPayload).Version }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ev := eventEnvelope("kanban."+c.name, c.payload)
			if v := c.version(ev); v != payloadVersion {
				t.Fatalf("Version = %d, want %d", v, payloadVersion)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func containsType(evs []event.Event, t string) bool {
	for _, e := range evs {
		if string(e.Type) == t {
			return true
		}
	}
	return false
}

func eventTypes(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = string(e.Type)
	}
	return out
}
