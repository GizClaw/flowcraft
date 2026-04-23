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

// publishAndWait pushes a payload onto the board's bus and waits until at
// least minSeq events have been recorded in the log, returning them in
// seq order. It exists so we don't sprinkle ad-hoc sleeps across tests.
func publishAndWait(t *testing.T, log *eventlog.SQLiteLog, board *kanban.Board, evType string, payload any, minSeq int64) []eventlog.Envelope {
	t.Helper()
	if err := board.Bus().Publish(context.Background(), event.Event{
		Type:    event.EventType(evType),
		Payload: payload,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		latest, err := log.LatestSeq(context.Background())
		if err != nil {
			t.Fatalf("latest seq: %v", err)
		}
		if latest >= minSeq {
			res, err := log.ReadAll(context.Background(), 0, 100)
			if err != nil {
				t.Fatalf("read all: %v", err)
			}
			return res.Events
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for seq>=%d", minSeq)
	return nil
}

func TestKanbanBridge_TranslatesAllFourTaskEvents(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-1")
	t.Cleanup(board.Close)

	kb, _, _, err := eventlog.BootKanbanWithBridge(context.Background(), log, board)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = kb.Close() })

	// task.submitted
	publishAndWait(t, log, board, kanban.EventTaskSubmitted, kanban.TaskSubmittedPayload{
		CardID:        "c1",
		TargetAgentID: "agent-x",
		Query:         "do thing",
		RuntimeID:     "rt-1",
	}, 1)
	// task.claimed
	publishAndWait(t, log, board, kanban.EventTaskClaimed, kanban.TaskClaimedPayload{
		CardID:        "c1",
		TargetAgentID: "agent-x",
		RuntimeID:     "rt-1",
	}, 2)
	// task.completed
	publishAndWait(t, log, board, kanban.EventTaskCompleted, kanban.TaskCompletedPayload{
		CardID:        "c1",
		TargetAgentID: "agent-x",
		RuntimeID:     "rt-1",
		Output:        "ok",
		ElapsedMs:     5,
	}, 3)
	// task.failed
	envs := publishAndWait(t, log, board, kanban.EventTaskFailed, kanban.TaskFailedPayload{
		CardID:        "c2",
		TargetAgentID: "agent-y",
		RuntimeID:     "rt-1",
		Error:         "boom",
		ElapsedMs:     2,
	}, 4)

	wantTypes := []string{"task.submitted", "task.claimed", "task.completed", "task.failed"}
	if len(envs) != len(wantTypes) {
		t.Fatalf("want %d envelopes, got %d", len(wantTypes), len(envs))
	}
	for i, env := range envs {
		if env.Type != wantTypes[i] {
			t.Fatalf("env[%d].Type=%q, want %q", i, env.Type, wantTypes[i])
		}
		if env.Partition != "runtime:rt-1" {
			t.Fatalf("env[%d].Partition=%q, want runtime:rt-1", i, env.Partition)
		}
		if env.Category != "business" {
			t.Fatalf("env[%d].Category=%q, want business", i, env.Category)
		}
	}

	var failed map[string]any
	if err := json.Unmarshal(envs[3].Payload, &failed); err != nil {
		t.Fatalf("unmarshal failed payload: %v", err)
	}
	if got := failed["card_id"]; got != "c2" {
		t.Fatalf("failed.card_id=%v, want c2", got)
	}
	if got := failed["error"]; got != "boom" {
		t.Fatalf("failed.error=%v, want boom", got)
	}
}

func TestKanbanBridge_AttachIsIdempotent(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-2")
	t.Cleanup(board.Close)

	kb := eventlog.NewKanbanBridge(log)
	if err := kb.Attach(context.Background(), board); err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = kb.Close() })
	if err := kb.Attach(context.Background(), board); err == nil {
		t.Fatal("second Attach should fail")
	}
}

// TestKanbanBridge_NoEventsBeforeAttach validates the §R3 DoD bullet 4:
// "restore 期间 board 发出的事件不进入 event_log". We simulate restore by
// publishing onto board.Bus() *before* Attach is called — those events
// must be silently dropped.
func TestKanbanBridge_NoEventsBeforeAttach(t *testing.T) {
	log := newTestLog(t)
	board := kanban.NewBoard("rt-3")
	t.Cleanup(board.Close)

	// "restore phase": no bridge yet, push a fake task.submitted.
	if err := board.Bus().Publish(context.Background(), event.Event{
		Type: event.EventType(kanban.EventTaskSubmitted),
		Payload: kanban.TaskSubmittedPayload{
			CardID: "ghost", RuntimeID: "rt-3",
		},
	}); err != nil {
		t.Fatalf("publish ghost: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	if seq, err := log.LatestSeq(context.Background()); err != nil {
		t.Fatalf("latest seq: %v", err)
	} else if seq != 0 {
		t.Fatalf("event_log non-empty before Attach (seq=%d)", seq)
	}

	kb := eventlog.NewKanbanBridge(log)
	if err := kb.Attach(context.Background(), board); err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = kb.Close() })

	// After Attach, only events published from this point forward should
	// land in event_log.
	publishAndWait(t, log, board, kanban.EventTaskSubmitted, kanban.TaskSubmittedPayload{
		CardID: "real", RuntimeID: "rt-3",
	}, 1)

	res, err := log.ReadAll(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(res.Events))
	}
	var p map[string]any
	if err := json.Unmarshal(res.Events[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p["card_id"] != "real" {
		t.Fatalf("card_id=%v, want real (ghost leaked)", p["card_id"])
	}
}
