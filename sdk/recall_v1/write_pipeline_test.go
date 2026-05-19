package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall_v1"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// TestSaveAsync_HandleJobRevalidatesScope pins issue #165: a payload
// that survives a durable JobQueue adapter or a direct queue
// Enqueue (bypassing SaveAsync) must still go through validateScope
// at consumption time. The unified [executeWrite] pipeline runs the
// gate; handleJob treats a validate-stage failure as permanent and
// dead-letters the job without retrying.
func TestSaveAsync_HandleJobRevalidatesScope(t *testing.T) {
	idx := memidx.New()
	queue := recall.NewMemoryJobQueue()
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithJobQueue(queue),
		recall.WithJobRetry(3, 10*time.Millisecond, 100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	// Bypass SaveAsync: enqueue a payload directly into the queue
	// with an empty UserID. SaveAsync would reject this at Enqueue;
	// a durable adapter that snapshots an older payload, or an
	// advanced caller talking to JobQueue directly, would not.
	badPayload := recall.JobPayload{
		Scope: recall.Scope{RuntimeID: "rt1"}, // missing UserID
		Messages: []llm.Message{{
			Role:  model.RoleUser,
			Parts: []model.Part{{Type: model.PartText, Text: "ignored"}},
		}},
	}
	id, err := queue.Enqueue(context.Background(), "ltm_rt1__global", badPayload)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Poll for the worker to consume + dead-letter the job.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := queue.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if rec.State == recall.JobDead {
			// Attempts should be 1 — the permanent-failure path
			// skipped the retry loop entirely.
			if rec.Attempts != 1 {
				t.Fatalf("job dead-lettered after %d attempts; want 1 (permanent fail skips retries)", rec.Attempts)
			}
			if rec.LastError == "" {
				t.Fatalf("dead job has no LastError; expected validateScope rejection")
			}
			return
		}
		if rec.State == recall.JobFailed {
			// Earlier behaviour would land the job in Failed
			// after maxAttempts retries. Acceptable as a less-
			// precise signal that the gate fires — but verify it
			// did not waste retries.
			if rec.Attempts > 1 {
				t.Fatalf("validate-stage failure burned %d retries; expected permanent fail-no-retry", rec.Attempts)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job did not dead-letter within 3s; final state never reached")
}

// TestExecuteWrite_SyncAsyncProduceSameIDs is a parity guard: the
// sync ([Memory.Save]) and async ([Memory.SaveAsync]) paths feed the
// SAME (scope, msgs) through the same [executeWrite] pipeline, so
// the deterministic entry IDs they assign must be byte-identical.
// Any future divergence — a pre-extract hook added to Save but not
// handleJob, an extractor option dropped on one side, …  — flips
// this assertion before it ships.
func TestExecuteWrite_SyncAsyncProduceSameIDs(t *testing.T) {
	ctx := context.Background()
	resp := `[{"content":"user owns a labrador named Lucky","entities":["lucky"]}]`

	// Sync path.
	idxSync := memidx.New()
	mSync, err := recall.New(idxSync,
		recall.WithRequireUserID(),
		recall.WithLLM(&stubLLM{resp: resp}),
	)
	if err != nil {
		t.Fatalf("New sync: %v", err)
	}
	defer mSync.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	msgs := []llm.Message{{
		Role:  model.RoleUser,
		Parts: []model.Part{{Type: model.PartText, Text: "I own a labrador named Lucky"}},
	}}
	syncRes, err := mSync.Save(ctx, scope, msgs)
	if err != nil {
		t.Fatalf("Save sync: %v", err)
	}
	if len(syncRes.EntryIDs) == 0 {
		t.Fatalf("sync Save returned no entry ids")
	}

	// Async path — fresh index and Memory so we compare ID
	// determinism, not row collision.
	idxAsync := memidx.New()
	mAsync, err := recall.New(idxAsync,
		recall.WithRequireUserID(),
		recall.WithLLM(&stubLLM{resp: resp}),
	)
	if err != nil {
		t.Fatalf("New async: %v", err)
	}
	defer mAsync.Close()

	jobID, err := mAsync.SaveAsync(ctx, scope, msgs)
	if err != nil {
		t.Fatalf("SaveAsync: %v", err)
	}
	jc, ok := mAsync.(recall.JobController)
	if !ok {
		t.Fatalf("Memory does not implement JobController")
	}
	st, err := jc.AwaitJob(ctx, jobID, 3*time.Second)
	if err != nil {
		t.Fatalf("AwaitJob: %v", err)
	}
	if st.State != recall.JobSucceeded {
		t.Fatalf("async job state = %s; LastError=%q", st.State, st.LastError)
	}
	if len(st.EntryIDs) != len(syncRes.EntryIDs) {
		t.Fatalf("async EntryIDs len %d != sync len %d", len(st.EntryIDs), len(syncRes.EntryIDs))
	}
	for i := range st.EntryIDs {
		if st.EntryIDs[i] != syncRes.EntryIDs[i] {
			t.Fatalf("entry id divergence at [%d]: sync=%s async=%s",
				i, syncRes.EntryIDs[i], st.EntryIDs[i])
		}
	}
}
