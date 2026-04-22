package eventlog_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/store"
)

// newTestLog spins up a tmpfile-backed SQLiteStore and wraps it as a Log.
// Migrations run as part of NewSQLiteStore.
func newTestLog(t testing.TB) *eventlog.SQLiteLog {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "events.db")
	s, err := store.NewSQLiteStore(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return eventlog.NewSQLiteLog(s.DB())
}

func mkDraft(seq int) eventlog.EnvelopeDraft {
	return eventlog.EnvelopeDraft{
		Partition: "runtime:rt-1",
		Type:      "task.submitted",
		Version:   1,
		Category:  "business",
		Payload:   map[string]any{"task_id": seq},
		Actor:     &eventlog.Actor{ID: "u-1", Kind: "user", RealmID: "realm-1"},
	}
}

func TestSQLiteLog_AtomicAssignsSeq(t *testing.T) {
	log := newTestLog(t)
	envs, err := log.Atomic(context.Background(), func(uow eventlog.UnitOfWork) error {
		return uow.Append(context.Background(), mkDraft(1), mkDraft(2), mkDraft(3))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 3 {
		t.Fatalf("want 3 envs, got %d", len(envs))
	}
	for i, e := range envs {
		if e.Seq != int64(i+1) {
			t.Fatalf("env %d seq=%d", i, e.Seq)
		}
	}
}

func TestSQLiteLog_RoundtripPreservesActor(t *testing.T) {
	log := newTestLog(t)
	envs, err := log.Atomic(context.Background(), func(uow eventlog.UnitOfWork) error {
		return uow.Append(context.Background(), mkDraft(1))
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := log.Read(context.Background(), "runtime:rt-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(got.Events))
	}
	a := got.Events[0].Actor
	if a == nil || a.ID != "u-1" || a.Kind != "user" || a.RealmID != "realm-1" {
		t.Fatalf("actor roundtrip lost data: %+v", a)
	}
	if got.Events[0].Seq != envs[0].Seq {
		t.Fatalf("seq mismatch")
	}
}

func TestSQLiteLog_SubscribeReplayThenLive(t *testing.T) {
	log := newTestLog(t)
	ctx := context.Background()

	// Seed history first.
	if _, err := log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return uow.Append(ctx, mkDraft(1), mkDraft(2), mkDraft(3))
	}); err != nil {
		t.Fatal(err)
	}

	sub, err := log.Subscribe(ctx, eventlog.SubscribeOptions{
		Partitions: []string{"runtime:rt-1"},
		Since:      eventlog.SinceBeginning,
		BufferSize: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	got := make([]int64, 0, 6)
	deadline := time.Now().Add(2 * time.Second)
	expect := 6
	go func() {
		// Append 3 more after a tiny delay so we exercise replay→live cut-over.
		time.Sleep(50 * time.Millisecond)
		_, _ = log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			return uow.Append(ctx, mkDraft(4), mkDraft(5), mkDraft(6))
		})
	}()
	for len(got) < expect && time.Now().Before(deadline) {
		select {
		case env := <-sub.C():
			got = append(got, env.Seq)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if len(got) != expect {
		t.Fatalf("want %d events, got %v", expect, got)
	}
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("seq order broken: %v", got)
		}
	}
}

func TestSQLiteLog_SubscribePartitionFilter(t *testing.T) {
	log := newTestLog(t)
	ctx := context.Background()
	other := mkDraft(1)
	other.Partition = "runtime:rt-2"
	if _, err := log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return uow.Append(ctx, mkDraft(1), other, mkDraft(2))
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := log.Subscribe(ctx, eventlog.SubscribeOptions{
		Partitions: []string{"runtime:rt-1"},
		Since:      eventlog.SinceBeginning,
		BufferSize: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	deadline := time.Now().Add(time.Second)
	var got []eventlog.Envelope
	for time.Now().Before(deadline) && len(got) < 2 {
		select {
		case env := <-sub.C():
			got = append(got, env)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	for _, e := range got {
		if e.Partition != "runtime:rt-1" {
			t.Fatalf("filter leaked: %v", e.Partition)
		}
	}
}

func TestSQLiteLog_RollbackDoesNotPersist(t *testing.T) {
	log := newTestLog(t)
	wantErr := "boom"
	_, err := log.Atomic(context.Background(), func(uow eventlog.UnitOfWork) error {
		_ = uow.Append(context.Background(), mkDraft(1))
		return errBoom(wantErr)
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	res, err := log.ReadAll(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 0 {
		t.Fatalf("expected rollback, got %d events", len(res.Events))
	}
}

type errBoom string

func (e errBoom) Error() string { return string(e) }
