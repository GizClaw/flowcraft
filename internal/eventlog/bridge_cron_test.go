package eventlog_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

// produceSchedulerCard creates a card produced by the scheduler so the cron
// bridge can recognise it as cron-triggered.
func produceSchedulerCard(t *testing.T, board *kanban.Board, scheduleID string) *kanban.Card {
	t.Helper()
	return board.Produce("task", "scheduler", kanban.TaskPayload{
		TargetAgentID: "agent-1",
		Query:         "scheduled",
	}, kanban.WithMeta("schedule_id", scheduleID))
}

func TestCronBridge_FiresOnSchedulerSubmissions(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-c1")
	t.Cleanup(board.Close)

	_, cb, err := eventlog.BootKanbanWithBridge(context.Background(), log, board)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = cb.Close() })

	card := produceSchedulerCard(t, board, "sched-1")

	if err := board.Bus().Publish(context.Background(), event.Event{
		Type: event.EventType(kanban.EventTaskSubmitted),
		Payload: kanban.TaskSubmittedPayload{
			CardID:        card.ID,
			TargetAgentID: "agent-1",
			Query:         "scheduled",
			RuntimeID:     "rt-c1",
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	envs := waitForEnvelopeType(t, log, "cron.rule.fired", 2*time.Second)
	if len(envs) != 1 {
		t.Fatalf("want 1 cron.rule.fired, got %d", len(envs))
	}
	got := envs[0]
	if got.Partition != "runtime:rt-c1" {
		t.Fatalf("partition=%q, want runtime:rt-c1", got.Partition)
	}
	if got.Category != "operational" {
		t.Fatalf("category=%q, want operational", got.Category)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["rule_id"] != "sched-1" {
		t.Fatalf("rule_id=%v, want sched-1", payload["rule_id"])
	}
	if _, ok := payload["fire_key"].(string); !ok {
		t.Fatalf("fire_key missing or not string: %v", payload["fire_key"])
	}
}

func TestCronBridge_IgnoresUserSubmissions(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-c2")
	t.Cleanup(board.Close)

	_, cb, err := eventlog.BootKanbanWithBridge(context.Background(), log, board)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = cb.Close() })

	// Card produced by a regular user (no schedule_id).
	card := board.Produce("task", "user-1", kanban.TaskPayload{TargetAgentID: "agent-1", Query: "ad-hoc"})

	if err := board.Bus().Publish(context.Background(), event.Event{
		Type: event.EventType(kanban.EventTaskSubmitted),
		Payload: kanban.TaskSubmittedPayload{
			CardID:    card.ID,
			RuntimeID: "rt-c2",
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// task.submitted will be appended by the kanban bridge but cron
	// bridge must not emit a fired envelope.
	time.Sleep(50 * time.Millisecond)
	envs := readEnvelopesByType(t, log, "cron.rule.fired")
	if len(envs) != 0 {
		t.Fatalf("want 0 cron.rule.fired for user submission, got %d", len(envs))
	}
}

// TestCronBridge_FireKeyIdempotent validates D.19: replaying the same
// scheduler submission must not double-publish cron.rule.fired.
func TestCronBridge_FireKeyIdempotent(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-c3")
	t.Cleanup(board.Close)

	_, cb, err := eventlog.BootKanbanWithBridge(context.Background(), log, board)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = cb.Close() })

	card := produceSchedulerCard(t, board, "sched-d19")

	for i := 0; i < 3; i++ {
		if err := board.Bus().Publish(context.Background(), event.Event{
			Type: event.EventType(kanban.EventTaskSubmitted),
			Payload: kanban.TaskSubmittedPayload{
				CardID:    card.ID,
				RuntimeID: "rt-c3",
			},
		}); err != nil {
			t.Fatalf("publish #%d: %v", i, err)
		}
	}

	envs := waitForEnvelopeType(t, log, "cron.rule.fired", 2*time.Second)
	if len(envs) != 1 {
		t.Fatalf("want exactly 1 cron.rule.fired (idempotency), got %d", len(envs))
	}
}

func TestCronBridge_PublishLifecycleHelpers(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-c4")
	t.Cleanup(board.Close)

	_, cb, err := eventlog.BootKanbanWithBridge(context.Background(), log, board)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = cb.Close() })

	rule := eventlog.CronRuleEvent{
		RuleID:        "sch-lifecycle",
		RuntimeID:     "rt-c4",
		Expression:    "*/5 * * * *",
		Timezone:      "Asia/Shanghai",
		TargetAgentID: "agent-1",
		Query:         "ping",
		Enabled:       true,
	}
	if err := cb.PublishRuleCreated(context.Background(), rule); err != nil {
		t.Fatalf("created: %v", err)
	}
	rule.Query = "pong"
	if err := cb.PublishRuleChanged(context.Background(), rule); err != nil {
		t.Fatalf("changed: %v", err)
	}
	disabled := rule
	disabled.Enabled = false
	disabled.DisabledAt = "2026-01-01T00:00:00Z"
	if err := cb.PublishRuleDisabled(context.Background(), disabled); err != nil {
		t.Fatalf("disabled: %v", err)
	}

	wantTypes := []string{"cron.rule.created", "cron.rule.changed", "cron.rule.disabled"}
	for _, evType := range wantTypes {
		envs := readEnvelopesByType(t, log, evType)
		if len(envs) != 1 {
			t.Fatalf("want 1 %s envelope, got %d", evType, len(envs))
		}
		if envs[0].Partition != "runtime:rt-c4" {
			t.Fatalf("%s partition=%q, want runtime:rt-c4", evType, envs[0].Partition)
		}
		if envs[0].Category != "business" {
			t.Fatalf("%s category=%q, want business", evType, envs[0].Category)
		}
	}
}

// ---- helpers ----

func waitForEnvelopeType(t *testing.T, log *eventlog.SQLiteLog, evType string, timeout time.Duration) []eventlog.Envelope {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		envs := readEnvelopesByType(t, log, evType)
		if len(envs) > 0 {
			return envs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for envelope type %q", evType)
	return nil
}

func readEnvelopesByType(t *testing.T, log *eventlog.SQLiteLog, evType string) []eventlog.Envelope {
	t.Helper()
	res, err := log.ReadAll(context.Background(), 0, 1000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out []eventlog.Envelope
	for _, env := range res.Events {
		if env.Type == evType {
			out = append(out, env)
		}
	}
	return out
}
