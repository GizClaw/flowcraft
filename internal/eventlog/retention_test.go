package eventlog_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// withFixedTimes fast-forwards eventlog.Time during the body so events get
// a deterministic ts.
func withFixedTimes(t *testing.T, ts string, body func()) {
	t.Helper()
	prev := eventlog.Time
	eventlog.Time = func() string { return ts }
	defer func() { eventlog.Time = prev }()
	body()
}

func appendDraft(t *testing.T, log *eventlog.SQLiteLog, partition, evType, category string) eventlog.Envelope {
	t.Helper()
	envs, err := log.Atomic(context.Background(), func(uow eventlog.UnitOfWork) error {
		return uow.Append(context.Background(), eventlog.EnvelopeDraft{
			Partition: partition,
			Type:      evType,
			Version:   1,
			Category:  category,
			Payload:   map[string]any{"hello": "world"},
		})
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return envs[0]
}

func TestRetention_DeletesExpiredCategoryRows(t *testing.T) {
	log := newTestLog(t)

	// Two volatile rows from "10 days ago" plus one fresh business row.
	old := time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	withFixedTimes(t, old, func() {
		appendDraft(t, log, "runtime:rt-r1", "agent.stream.delta", "volatile")
		appendDraft(t, log, "runtime:rt-r1", "agent.stream.delta", "volatile")
	})
	appendDraft(t, log, "runtime:rt-r1", "task.submitted", "business") // fresh

	cfg := eventlog.RetentionConfig{
		Categories: map[string]time.Duration{
			"volatile": 24 * time.Hour, // anything older than 1d goes
			"business": 90 * 24 * time.Hour,
		},
		BatchSize: 100,
	}

	if err := eventlog.RunRetention(context.Background(), log, cfg, log.Checkpoints()); err != nil {
		t.Fatalf("retention: %v", err)
	}

	res, err := log.ReadAll(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("after retention want 1 row, got %d", len(res.Events))
	}
	if res.Events[0].Category != "business" {
		t.Fatalf("survivor category=%q, want business", res.Events[0].Category)
	}
}

func TestRetention_RespectsCheckpointMin(t *testing.T) {
	log := newTestLog(t)

	old := time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	var firstSeq, secondSeq int64
	withFixedTimes(t, old, func() {
		firstSeq = appendDraft(t, log, "runtime:rt-r2", "agent.stream.delta", "volatile").Seq
		secondSeq = appendDraft(t, log, "runtime:rt-r2", "agent.stream.delta", "volatile").Seq
	})

	// Register a projector with checkpoint = secondSeq. The query uses
	// seq<minCP, so only firstSeq (< secondSeq) is eligible for deletion;
	// secondSeq must survive even though both rows are old enough.
	if _, err := log.Atomic(context.Background(), func(uow eventlog.UnitOfWork) error {
		return uow.CheckpointSet(context.Background(), "p1", secondSeq)
	}); err != nil {
		t.Fatalf("checkpoint set: %v", err)
	}
	_ = firstSeq

	cfg := eventlog.RetentionConfig{
		Categories: map[string]time.Duration{
			"volatile": 24 * time.Hour,
		},
		BatchSize: 100,
	}
	if err := eventlog.RunRetention(context.Background(), log, cfg, log.Checkpoints()); err != nil {
		t.Fatalf("retention: %v", err)
	}

	res, err := log.ReadAll(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("want 1 surviving row (seq>checkpoint), got %d", len(res.Events))
	}
	if res.Events[0].Seq != secondSeq {
		t.Fatalf("survivor seq=%d, want %d", res.Events[0].Seq, secondSeq)
	}
}

func TestRetention_PermanentCategoryNeverDeletes(t *testing.T) {
	log := newTestLog(t)

	old := time.Now().Add(-365 * 24 * time.Hour).Format(time.RFC3339Nano)
	withFixedTimes(t, old, func() {
		appendDraft(t, log, "realm:r1", "realm.created", "permanent")
	})

	cfg := eventlog.RetentionConfig{
		Categories: map[string]time.Duration{
			"permanent": 0,
		},
		BatchSize: 100,
	}
	if err := eventlog.RunRetention(context.Background(), log, cfg, log.Checkpoints()); err != nil {
		t.Fatalf("retention: %v", err)
	}
	res, _ := log.ReadAll(context.Background(), 0, 100)
	if len(res.Events) != 1 {
		t.Fatalf("permanent row was deleted: got %d rows", len(res.Events))
	}
}
