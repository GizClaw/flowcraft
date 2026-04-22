package eventlog_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// BenchmarkAppend hammers single-row Atomic appends; we want sub-millisecond
// medians so cron/agent firing rates aren't bottlenecked.
func BenchmarkAppend(b *testing.B) {
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

// BenchmarkSubscribeFanout exercises the post-commit fan-out path; results
// describe how much overhead one extra subscriber adds per Atomic.
func BenchmarkSubscribeFanout(b *testing.B) {
	log := newTestLog(b)
	ctx := context.Background()
	const subs = 8
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
		// Drain in background so producer can keep going.
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
}
