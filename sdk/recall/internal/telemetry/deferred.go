package telemetry

import (
	"sync"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Deferred buffers OnStage events while one or more Hold scopes are
// active and flushes them when the last matching Flush drops the
// hold depth to zero. Concurrent partition writes each call
// Hold/Flush in pairs; ref-counting prevents one scope's Flush from
// releasing buffering while another scope still holds the write lock.
type Deferred struct {
	Inner port.TelemetryHook

	mu        sync.Mutex
	holdCount int
	pending   []diagnostic.StageDiagnostic
}

// NewDeferred wraps hook. When hook is nil, Flush and OnStage are no-ops.
func NewDeferred(hook port.TelemetryHook) *Deferred {
	return &Deferred{Inner: hook}
}

// Hold starts (or nests) a buffered telemetry scope.
func (d *Deferred) Hold() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.holdCount++
	d.mu.Unlock()
}

// Flush ends one Hold scope. Buffered events are delivered to Inner
// only when the hold depth returns to zero.
func (d *Deferred) Flush() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.holdCount > 0 {
		d.holdCount--
	}
	shouldFlush := d.holdCount == 0
	var pending []diagnostic.StageDiagnostic
	if shouldFlush {
		pending = d.pending
		d.pending = nil
	}
	inner := d.Inner
	d.mu.Unlock()
	if !shouldFlush || inner == nil {
		return
	}
	for _, ev := range pending {
		inner.OnStage(ev)
	}
}

// OnStage implements port.TelemetryHook.
func (d *Deferred) OnStage(ev diagnostic.StageDiagnostic) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.holdCount > 0 {
		d.pending = append(d.pending, ev)
		d.mu.Unlock()
		return
	}
	inner := d.Inner
	d.mu.Unlock()
	if inner != nil {
		inner.OnStage(ev)
	}
}

var _ port.TelemetryHook = (*Deferred)(nil)
