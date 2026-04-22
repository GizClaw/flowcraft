package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdkx/recall/jobqueue/sqlite"
)

func TestEnqueueLeaseComplete(t *testing.T) {
	ctx := context.Background()
	q, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	id, err := q.Enqueue(ctx, "ns1", recall.JobPayload{
		Scope:    recall.Scope{RuntimeID: "rt1", UserID: "u1"},
		Messages: []llm.Message{},
	})
	if err != nil {
		t.Fatal(err)
	}

	rec, ok, err := q.Lease(ctx, time.Now())
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if rec.ID != id {
		t.Fatalf("id mismatch")
	}

	if err := q.Complete(ctx, id, []string{"e1", "e2"}); err != nil {
		t.Fatal(err)
	}
	got, err := q.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != recall.JobSucceeded {
		t.Fatalf("state=%s", got.State)
	}
	if len(got.EntryIDs) != 2 {
		t.Fatalf("entry_ids=%v", got.EntryIDs)
	}
}

func TestRescheduleAndBackoff(t *testing.T) {
	ctx := context.Background()
	q, _ := sqlite.Open(":memory:")
	defer q.Close()

	id, _ := q.Enqueue(ctx, "ns1", recall.JobPayload{Scope: recall.Scope{RuntimeID: "rt"}})
	_, ok, _ := q.Lease(ctx, time.Now())
	if !ok {
		t.Fatal("first lease failed")
	}
	future := time.Now().Add(2 * time.Second)
	if err := q.Reschedule(ctx, id, future, "boom"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := q.Lease(ctx, time.Now()); ok {
		t.Fatal("should not lease yet")
	}
	if _, ok, _ := q.Lease(ctx, future.Add(time.Millisecond)); !ok {
		t.Fatal("should lease after next_run_at")
	}
}

func TestCrashRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.db")

	q1, _ := sqlite.Open(path)
	id, _ := q1.Enqueue(ctx, "ns", recall.JobPayload{Scope: recall.Scope{RuntimeID: "rt"}})
	if _, ok, _ := q1.Lease(ctx, time.Now()); !ok {
		t.Fatal("lease before crash failed")
	}
	// Simulate crash: close without Complete.
	_ = q1.Close()

	q2, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer q2.Close()

	rec, ok, err := q2.Lease(ctx, time.Now())
	if err != nil || !ok {
		t.Fatalf("re-lease after recovery failed: ok=%v err=%v", ok, err)
	}
	if rec.ID != id {
		t.Fatalf("recovered id mismatch")
	}
}

func TestEndToEndWithLTM(t *testing.T) {
	ctx := context.Background()
	q, _ := sqlite.Open(":memory:")
	defer q.Close()

	idx := memidx.New()
	m, _ := recall.New(idx,
		recall.WithLLM(&fakeLLM{resp: `[{"content":"persisted fact","categories":["profile"]}]`}),
		recall.WithJobQueue(q),
		recall.WithRequireUserID(),
		recall.WithAsyncWorkers(1),
	)
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1", AgentID: "bot"}
	id, err := m.SaveAsync(ctx, scope, []llm.Message{
		{Role: "user", Parts: []llm.Part{{Type: "text", Text: "hi"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.AwaitJob(ctx, id, 5*time.Second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if st.State != recall.JobSucceeded {
		t.Fatalf("state=%s err=%s", st.State, st.LastError)
	}
}
