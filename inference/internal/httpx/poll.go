package httpx

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Await drives one asynchronous task to its terminal answer.
//
// A vendor that accepts a job and answers later — a video render, a long
// transcription — has the same loop in every driver: ask for the task, look at
// its state, sleep, ask again, and stop the first time the state is terminal.
// Only the state names and the query URL are the vendor's, which is what Done
// supplies.
type Await[T any] struct {
	// Op is the query the poller issues. Its Path may be absolute, because a
	// vendor may hand back a task URL.
	Op Op
	// Interval is the pause between queries. Zero uses DefaultPollInterval.
	Interval time.Duration
	// Budget bounds the whole wait. Zero means the caller's context is the only
	// bound.
	Budget time.Duration
	// Done reads the decoded answer: terminal reports that the task stopped,
	// and err is what the caller gets when it stopped by failing.
	Done func(T) (terminal bool, err error)
}

// DefaultPollInterval is the pause between task queries when a call does not say
// otherwise. Task surfaces are slow by nature; polling faster only spends rate
// limit.
const DefaultPollInterval = 2 * time.Second

// Await drives q until it reports a terminal state.
func Poll[T any](ctx context.Context, c *Client, q Await[T]) (T, error) {
	var zero T
	if q.Done == nil {
		return zero, errdefs.Validationf("poll: a completion check is required")
	}
	interval := q.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	deadline := time.Time{}
	if q.Budget > 0 {
		deadline = time.Now().Add(q.Budget)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return zero, errdefs.FromContext(ctx.Err())
		case <-timer.C:
		}
		var value T
		if err := c.JSON(ctx, q.Op, &value); err != nil {
			return zero, err
		}
		terminal, err := q.Done(value)
		if err != nil {
			return zero, err
		}
		if terminal {
			return value, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return zero, errdefs.NotAvailablef(
				"poll: the task did not finish within %s", q.Budget)
		}
		timer.Reset(interval)
	}
}
