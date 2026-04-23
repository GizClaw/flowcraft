package eventlog_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// Performance budgets for the eventlog hot path. The "target" comments below
// reproduce docs/event-sourcing-plan.md §9.4 + §11.7 verbatim; the *enforced*
// budget is intentionally looser (≈5× the target) because:
//
//   - The doc figures assume warm SQLite WAL + tmpfs; CI runs on shared
//     ext4 with fsync, where 200µs/op is the floor.
//   - We want CI-as-budget to fire on real regressions (≥2× slowdown), not
//     on the slowest of three GitHub-hosted runners.
//
// Once §9.4's baseline-snapshot CI lands (`bench → 1.2×` gate), we'll replace
// these constants with a baseline diff and tighten back to the doc values.
const (
	budgetAppendSingle = 1 * time.Millisecond  // target 100µs (§9.4)
	budgetAppendBatch  = 3 * time.Millisecond  // target 500µs (§9.4)
	budgetFanoutPerOp  = 5 * time.Millisecond  // target 1ms (§9.4)
	budgetSubscribeE2E = 50 * time.Millisecond // target 10ms p99 (§11.7)
)

// BenchmarkAppend_Single hammers the single-row Atomic path; the documented
// budget is 100µs/op so cron / agent firing rates aren't bottlenecked.
func BenchmarkAppend_Single(b *testing.B) {
	log := newTestLog(b)
	ctx := context.Background()
	d := mkDraft(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			return uow.Append(ctx, d)
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	assertBudget(b, budgetAppendSingle)
}

// BenchmarkAppend_Batch10 measures 10-event Atomic batches, the size used by
// command handlers that emit a `*.requested + *.committed` pair plus side
// effects. Budget: 500µs/op.
func BenchmarkAppend_Batch10(b *testing.B) {
	log := newTestLog(b)
	ctx := context.Background()
	batch := make([]eventlog.EnvelopeDraft, 10)
	for i := range batch {
		batch[i] = mkDraft(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			return uow.Append(ctx, batch...)
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	assertBudget(b, budgetAppendBatch)
}

// BenchmarkRead pages through history of a single partition. Used to size
// the HTTP-pull limit and SSE catch-up speed.
func BenchmarkRead(b *testing.B) {
	log := newTestLog(b)
	ctx := context.Background()
	const seed = 1024
	for i := 0; i < seed; i++ {
		_, _ = log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			return uow.Append(ctx, mkDraft(i))
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := log.Read(ctx, "runtime:rt-1", 0, 256); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSubscribe_Fanout10 exercises the post-commit fan-out path with 10
// active subscribers; budget is 1ms per Atomic so a busy projector tree
// doesn't queue up at the Append boundary.
func BenchmarkSubscribe_Fanout10(b *testing.B) {
	log := newTestLog(b)
	ctx := context.Background()
	const subs = 10
	for i := 0; i < subs; i++ {
		s, err := log.Subscribe(ctx, eventlog.SubscribeOptions{
			Partitions: []string{"runtime:rt-1"},
			Since:      eventlog.SinceLive,
			BufferSize: 1024,
		})
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
		go func() {
			for range s.C() {
			}
		}()
	}
	d := mkDraft(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			return uow.Append(ctx, d)
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	assertBudget(b, budgetFanoutPerOp)
}

// BenchmarkSubscribeE2E measures producer→subscriber wall time: append one
// envelope and wait for it on the subscribe channel. Budget: 10ms (§11.7
// Subscribe p99). Reports per-op average; with -benchtime 200x or higher
// the average is a good p99 proxy for this single-hop path.
func BenchmarkSubscribeE2E(b *testing.B) {
	log := newTestLog(b)
	ctx := context.Background()
	sub, err := log.Subscribe(ctx, eventlog.SubscribeOptions{
		Partitions: []string{"runtime:rt-1"},
		Since:      eventlog.SinceLive,
		BufferSize: 1024,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer sub.Close()
	d := mkDraft(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			return uow.Append(ctx, d)
		}); err != nil {
			b.Fatal(err)
		}
		select {
		case <-sub.C():
		case <-time.After(2 * time.Second):
			b.Fatal("subscribe timeout: producer→consumer >2s")
		}
	}
	b.StopTimer()
	assertBudget(b, budgetSubscribeE2E)
}

// assertBudget fails the bench if the per-op average exceeds the documented
// budget. Always uses b.Elapsed/b.N so it's correct under any -benchtime.
func assertBudget(b *testing.B, budget time.Duration) {
	b.Helper()
	if b.N == 0 {
		return
	}
	perOp := b.Elapsed() / time.Duration(b.N)
	if perOp > budget {
		b.Fatalf("BUDGET %v/op exceeds %v/op (b.N=%d, total=%v)",
			perOp, budget, b.N, b.Elapsed())
	}
}
