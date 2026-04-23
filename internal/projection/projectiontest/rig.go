// Package projectiontest is the standard test rig for projector authors.
// It bundles a MemoryLog, a Manager, and assertion helpers so projector
// tests boil down to "seed events, assert read-model state".
package projectiontest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/eventlogtest"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// Rig wires together everything a projector unit test needs: an in-memory
// log, a manager, and a list of registered projectors.
type Rig struct {
	tb         testing.TB
	Log        *eventlogtest.MemoryLog
	Manager    *projection.Manager
	clock      eventlogtest.Clock
	mu         sync.Mutex
	cancels    []context.CancelFunc
	started    bool
	projectors map[string]projection.Projector
}

// NewRig builds a fresh Rig with a MemoryLog and a Manager (5s ready timeout).
// The MemoryLog uses eventlogtest.SystemClock by default; tests that need
// deterministic timestamps should call Rig.WithClock before Seed.
func NewRig(tb testing.TB) *Rig {
	tb.Helper()
	log := eventlogtest.NewMemoryLog()
	mgr := projection.NewManager(projection.ManagerConfig{ReadyTimeout: 5 * time.Second})
	r := &Rig{
		tb:         tb,
		Log:        log,
		Manager:    mgr,
		clock:      eventlogtest.SystemClock,
		projectors: map[string]projection.Projector{},
	}
	tb.Cleanup(r.Stop)
	return r
}

// WithClock substitutes the wall clock used by the underlying MemoryLog so
// that envelope.ts becomes deterministic. Must be called before Start.
func (r *Rig) WithClock(c eventlogtest.Clock) *Rig {
	r.tb.Helper()
	if r.started {
		r.tb.Fatalf("Rig.WithClock: cannot change clock after Start")
	}
	if c == nil {
		return r
	}
	r.clock = c
	r.Log.WithClock(c)
	return r
}

// Clock returns the clock currently driving the underlying MemoryLog. Useful
// for tests that need to share the clock with code under test.
func (r *Rig) Clock() eventlogtest.Clock { return r.clock }

// Register adds a projector to the manager (must be called before Start).
func (r *Rig) Register(p projection.Projector, dependsOn []string, opts ...projection.RegisterOption) {
	r.tb.Helper()
	if err := r.Manager.RegisterProjector(p, dependsOn, opts...); err != nil {
		r.tb.Fatalf("Rig.Register: %v", err)
	}
	r.projectors[p.Name()] = p
}

// Projector returns the registered projector with the given name, or nil if
// no such projector has been registered. Tests use this to reach into a
// projector's read-model state without re-plumbing the constructor return.
func (r *Rig) Projector(name string) projection.Projector {
	if p, ok := r.projectors[name]; ok {
		return p
	}
	return r.Manager.ProjectorByName(name)
}

// Start launches the manager; subsequent Register calls fail.
func (r *Rig) Start(ctx context.Context) {
	r.tb.Helper()
	if r.started {
		return
	}
	r.started = true
	if err := r.Manager.Start(ctx, r.Log); err != nil {
		r.tb.Fatalf("Rig.Start: %v", err)
	}
}

// Seed appends drafts to the underlying MemoryLog.
func (r *Rig) Seed(drafts ...eventlog.EnvelopeDraft) []eventlog.Envelope {
	r.tb.Helper()
	return r.Log.Append(r.tb, drafts...)
}

// WaitReady blocks until every projector reports ready or the deadline elapses.
func (r *Rig) WaitReady(ctx context.Context, timeout time.Duration) {
	r.tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.Manager.IsAllReady() {
			return
		}
		select {
		case <-ctx.Done():
			r.tb.Fatalf("Rig.WaitReady: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	r.tb.Fatalf("Rig.WaitReady: not all projectors ready within %s; status=%+v",
		timeout, r.Manager.Status())
}

// WaitCheckpoint blocks until the named projector's checkpoint reaches at
// least seq, or fails the test on timeout. This is the preferred way to
// synchronize on "the projector has consumed event seq=N" because it polls
// the runner's atomic status snapshot rather than fishing through MemoryLog
// (whose CheckpointStore is intentionally a no-op).
func (r *Rig) WaitCheckpoint(name string, seq int64, timeout time.Duration) {
	r.tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, st := range r.Manager.Status() {
			if st.Name == name && st.CheckpointSeq >= seq {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.tb.Fatalf("Rig.WaitCheckpoint(%q, %d): not reached within %s; status=%+v",
		name, seq, timeout, r.Manager.Status())
}

// Stop tears down the manager (called automatically by t.Cleanup).
func (r *Rig) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cancels {
		c()
	}
	r.Manager.Stop()
}

// Eventually polls cond every 10ms until it returns true or timeout fires.
// Useful for asserting projector side-effects asynchronously.
func Eventually(tb testing.TB, timeout time.Duration, cond func() bool, msg string) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tb.Fatalf("Eventually timed out: %s", msg)
}
