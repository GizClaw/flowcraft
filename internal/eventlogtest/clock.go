package eventlogtest

import (
	"sync/atomic"
	"time"
)

// Clock is the small abstraction used by MemoryLog to stamp envelope.ts.
// SystemClock returns wall time in RFC3339Nano; FixedClock makes tests
// deterministic by advancing time only when the test asks it to.
type Clock interface {
	Now() string
}

type systemClock struct{}

func (systemClock) Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// SystemClock returns the wall clock; safe for benchmarks.
var SystemClock Clock = systemClock{}

// FixedClock returns a deterministic clock that starts at start and advances
// by step on every call to Now().
func FixedClock(start time.Time, step time.Duration) Clock {
	c := &fixedClock{step: step}
	c.now.Store(start.UnixNano())
	return c
}

type fixedClock struct {
	now  atomic.Int64
	step time.Duration
}

func (c *fixedClock) Now() string {
	cur := c.now.Load()
	c.now.Add(int64(c.step))
	return time.Unix(0, cur).UTC().Format(time.RFC3339Nano)
}
