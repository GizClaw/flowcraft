package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdkx/recall/jobqueue/sqlite"
)

// BenchmarkEnqueue measures raw Enqueue throughput against a freshly opened
// SQLite queue. Reports ops/sec via b.ReportMetric. WAL is on so each insert
// is one page write under contention; values cap roughly at fsync rate.
func BenchmarkEnqueue(b *testing.B) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"memory", ":memory:"},
		{"file", filepath.Join(b.TempDir(), "jobs.db")},
	} {
		b.Run(tc.name, func(b *testing.B) {
			q, err := sqlite.Open(tc.path)
			if err != nil {
				b.Fatal(err)
			}
			defer q.Close()
			ctx := context.Background()
			payload := recall.JobPayload{
				Scope:    recall.Scope{RuntimeID: "rt", UserID: "u"},
				Messages: []llm.Message{},
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := q.Enqueue(ctx, "ns", payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLeaseComplete measures the lease+complete loop a worker actually
// runs (drains a pre-populated backlog). The drain rate sets the effective
// SaveAsync ceiling once a queue is hot.
func BenchmarkLeaseComplete(b *testing.B) {
	q, err := sqlite.Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer q.Close()
	ctx := context.Background()
	payload := recall.JobPayload{Scope: recall.Scope{RuntimeID: "rt", UserID: "u"}}

	for i := 0; i < b.N; i++ {
		if _, err := q.Enqueue(ctx, "ns", payload); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	now := time.Now()
	for i := 0; i < b.N; i++ {
		rec, ok, err := q.Lease(ctx, now)
		if err != nil || !ok {
			b.Fatalf("lease: ok=%v err=%v", ok, err)
		}
		if err := q.Complete(ctx, rec.ID, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSaveAsync_EndToEnd measures wall-clock throughput of
// recall.SaveAsync → SQLite enqueue → background worker drain → Complete,
// the path that production agents actually exercise. The fake LLM returns a
// single canned fact so the benchmark isolates queue + persistence overhead
// from extractor latency.
//
// Run with -benchtime=Nx (e.g. 200x) since each op blocks on the worker
// finishing the underlying job; default time-based -benchtime=1s tends to
// undershoot on slow CI hardware.
func BenchmarkSaveAsync_EndToEnd(b *testing.B) {
	for _, workers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			q, err := sqlite.Open(":memory:")
			if err != nil {
				b.Fatal(err)
			}
			defer q.Close()

			idx := memidx.New()
			m, err := recall.New(idx,
				recall.WithLLM(&fakeLLM{resp: `[{"content":"f","categories":["profile"]}]`}),
				recall.WithJobQueue(q),
				recall.WithRequireUserID(),
				recall.WithAsyncWorkers(workers),
			)
			if err != nil {
				b.Fatal(err)
			}
			defer m.Close()

			jc, ok := m.(recall.JobController)
			if !ok {
				b.Fatal("recall.Memory does not implement JobController")
			}

			scope := recall.Scope{RuntimeID: "rt", UserID: "u", AgentID: "bot"}
			msgs := []llm.Message{
				{Role: "user", Parts: []llm.Part{{Type: "text", Text: "hello"}}},
			}

			ctx := context.Background()
			b.ResetTimer()

			var wg sync.WaitGroup
			var failed atomic.Int64
			wg.Add(b.N)
			for i := 0; i < b.N; i++ {
				id, err := m.SaveAsync(ctx, scope, msgs)
				if err != nil {
					b.Fatal(err)
				}
				go func(jid recall.JobID) {
					defer wg.Done()
					st, err := jc.AwaitJob(ctx, jid, 30*time.Second)
					if err != nil || st.State != recall.JobSucceeded {
						failed.Add(1)
					}
				}(id)
			}
			wg.Wait()
			if n := failed.Load(); n > 0 {
				b.Fatalf("%d/%d jobs failed", n, b.N)
			}
		})
	}
}
