package background

import (
	"sync"
	"time"
)

// Debouncer emits one signal after a quiet period.
//
// Reset arms or re-arms the timer. The timer callback only attempts to send on
// C; long-running work should happen in the owner loop that receives the signal.
// C has capacity 1, so signals coalesce when the owner is busy.
type Debouncer struct {
	delay time.Duration
	c     chan struct{}

	mu      sync.Mutex
	timer   *time.Timer
	gen     uint64
	stopped bool
}

// NewDebouncer creates a Debouncer. Non-positive delays are normalized to 1ns
// so Reset still schedules work asynchronously through the timer path.
func NewDebouncer(delay time.Duration) *Debouncer {
	if delay <= 0 {
		delay = time.Nanosecond
	}
	return &Debouncer{
		delay: delay,
		c:     make(chan struct{}, 1),
	}
}

// C returns the signal channel consumed by the owner loop.
func (d *Debouncer) C() <-chan struct{} {
	if d == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return d.c
}

// Reset schedules a signal after the quiet period. It returns false after Stop.
func (d *Debouncer) Reset() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	d.gen++
	if d.timer != nil {
		d.timer.Stop()
	}
	d.drainLocked()
	gen := d.gen
	d.timer = time.AfterFunc(d.delay, func() { d.fire(gen) })
	return true
}

// Stop cancels future signals and drains any pending signal.
func (d *Debouncer) Stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.stopped = true
	d.gen++
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.drainLocked()
}

func (d *Debouncer) fire(gen uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || gen != d.gen {
		return
	}
	select {
	case d.c <- struct{}{}:
	default:
	}
}

func (d *Debouncer) drainLocked() {
	for {
		select {
		case <-d.c:
		default:
			return
		}
	}
}
