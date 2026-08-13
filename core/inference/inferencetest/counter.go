package inferencetest

import "sync/atomic"

// Counter is a race-safe probe for compiler, transport, and session calls.
type Counter struct {
	value atomic.Int64
}

func (c *Counter) Add(delta int64) int64 { return c.value.Add(delta) }
func (c *Counter) Inc() int64            { return c.Add(1) }
func (c *Counter) Load() int64           { return c.value.Load() }
