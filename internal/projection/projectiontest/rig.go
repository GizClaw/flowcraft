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
	tb      testing.TB
	Log     *eventlogtest.MemoryLog
	Manager *projection.Manager
	mu      sync.Mutex
	cancels []context.CancelFunc
	started bool
}

// NewRig builds a fresh Rig with a MemoryLog and a Manager (5s ready timeout).
func NewRig(tb testing.TB) *Rig {
	tb.Helper()
	log := eventlogtest.NewMemoryLog()
	mgr := projection.NewManager(projection.ManagerConfig{ReadyTimeout: 5 * time.Second})
	r := &Rig{tb: tb, Log: log, Manager: mgr}
	tb.Cleanup(r.Stop)
	return r
}

// Register adds a projector to the manager (must be called before Start).
func (r *Rig) Register(p projection.Projector, dependsOn []string, opts ...projection.RegisterOption) {
	r.tb.Helper()
	if err := r.Manager.RegisterProjector(p, dependsOn, opts...); err != nil {
		r.tb.Fatalf("Rig.Register: %v", err)
	}
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
