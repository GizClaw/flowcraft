package projectiontest

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// FakeProjector is a configurable projector used by Rig tests. It records the
// sequence of envelopes it sees and exposes hooks to inject failures, observe
// readiness transitions, and assert ordering.
type FakeProjector struct {
	NameStr     string
	SubsList    []string
	Mode        projection.RestoreMode
	PartList    []string

	mu       sync.Mutex
	Applied  []eventlog.Envelope
	OnApply  func(env eventlog.Envelope) error
	OnReadyF func(ctx context.Context) error

	readyCalls atomic.Int64
}

// Name returns the projector name.
func (f *FakeProjector) Name() string { return f.NameStr }

// Subscribes returns the configured event types.
func (f *FakeProjector) Subscribes() []string { return f.SubsList }

// RestoreMode returns the configured restore mode.
func (f *FakeProjector) RestoreMode() projection.RestoreMode { return f.Mode }

// Partitions returns the configured partition filter; declared so the runner
// detects PartitionFilter and forwards Partitions to Subscribe.
func (f *FakeProjector) Partitions() []string { return f.PartList }

// Apply records the envelope and runs the optional OnApply hook (which may
// return an error to test retry/DLT paths).
func (f *FakeProjector) Apply(_ context.Context, _ eventlog.UnitOfWork, env eventlog.Envelope) error {
	if f.OnApply != nil {
		if err := f.OnApply(env); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.Applied = append(f.Applied, env)
	f.mu.Unlock()
	return nil
}

// OnReady is invoked once the projector becomes ready.
func (f *FakeProjector) OnReady(ctx context.Context) error {
	f.readyCalls.Add(1)
	if f.OnReadyF != nil {
		return f.OnReadyF(ctx)
	}
	return nil
}

// AppliedSeqs returns the seqs the projector has observed, in apply order.
func (f *FakeProjector) AppliedSeqs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.Applied))
	for i, e := range f.Applied {
		out[i] = e.Seq
	}
	return out
}

// ReadyCalls reports how many times OnReady has fired (must be exactly 1
// across the lifetime of a healthy projector).
func (f *FakeProjector) ReadyCalls() int64 { return f.readyCalls.Load() }
