package eventlogtest

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// Fixture is the standard scaffolding used by R2 unit tests: a MemoryLog,
// a deterministic clock, plus helpers to seed events and to wait for
// subscribers to drain them.
type Fixture struct {
	tb  testing.TB
	Log *MemoryLog
}

// NewFixture builds a Fixture with the given clock. Pass nil to use the
// SystemClock; pass FixedClock(...) for deterministic tests.
func NewFixture(tb testing.TB, c Clock) *Fixture {
	tb.Helper()
	if c == nil {
		c = SystemClock
	}
	return &Fixture{tb: tb, Log: NewMemoryLog().WithClock(c)}
}

// Seed appends drafts and returns the resulting envelopes; on error fails the
// test immediately.
func (f *Fixture) Seed(drafts ...eventlog.EnvelopeDraft) []eventlog.Envelope {
	f.tb.Helper()
	envs, err := f.Log.Atomic(context.Background(), func(uow eventlog.UnitOfWork) error {
		return uow.Append(context.Background(), drafts...)
	})
	if err != nil {
		f.tb.Fatalf("Fixture.Seed: %v", err)
	}
	return envs
}

// Subscribe subscribes with the given options or fails the test.
func (f *Fixture) Subscribe(opts eventlog.SubscribeOptions) eventlog.Subscription {
	f.tb.Helper()
	sub, err := f.Log.Subscribe(context.Background(), opts)
	if err != nil {
		f.tb.Fatalf("Fixture.Subscribe: %v", err)
	}
	return sub
}
