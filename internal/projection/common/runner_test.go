package projection_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	"github.com/GizClaw/flowcraft/internal/projection/projectiontest"
)

func draft(p, t string) eventlog.EnvelopeDraft {
	return eventlog.EnvelopeDraft{Partition: p, Type: t, Version: 1, Category: "business", Payload: map[string]any{}}
}

func TestRunner_ReplayThenLive(t *testing.T) {
	rig := projectiontest.NewRig(t)
	fp := &projectiontest.FakeProjector{
		NameStr:  "fake.projector",
		SubsList: []string{"task.submitted"},
		Mode:     projection.RestoreReplay,
	}
	rig.Register(fp, nil)
	rig.Seed(draft("runtime:rt-1", "task.submitted"), draft("runtime:rt-1", "task.submitted"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rig.Start(ctx)
	rig.WaitReady(ctx, 2*time.Second)

	rig.Seed(draft("runtime:rt-1", "task.submitted"))

	projectiontest.Eventually(t, time.Second, func() bool {
		return len(fp.AppliedSeqs()) == 3
	}, "want 3 applied envelopes")

	if calls := fp.ReadyCalls(); calls != 1 {
		t.Fatalf("expected exactly 1 OnReady call, got %d", calls)
	}
}

func TestRunner_PartitionFilter(t *testing.T) {
	rig := projectiontest.NewRig(t)
	fp := &projectiontest.FakeProjector{
		NameStr:  "fake.projector",
		SubsList: []string{"task.submitted"},
		PartList: []string{"runtime:rt-1"},
		Mode:     projection.RestoreReplay,
	}
	rig.Register(fp, nil)
	rig.Seed(
		draft("runtime:rt-1", "task.submitted"),
		draft("runtime:rt-2", "task.submitted"),
		draft("runtime:rt-1", "task.submitted"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rig.Start(ctx)
	rig.WaitReady(ctx, 2*time.Second)

	projectiontest.Eventually(t, time.Second, func() bool {
		return len(fp.AppliedSeqs()) == 2
	}, "want exactly 2 applied envelopes (rt-1 only)")
}

func TestManager_TopologicalOrder(t *testing.T) {
	rig := projectiontest.NewRig(t)

	a := &projectiontest.FakeProjector{NameStr: "a", SubsList: []string{"task.submitted"}, Mode: projection.RestoreReplay}
	b := &projectiontest.FakeProjector{NameStr: "b", SubsList: []string{"task.submitted"}, Mode: projection.RestoreReplay}
	rig.Register(a, nil)
	rig.Register(b, []string{"a"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rig.Start(ctx)
	rig.WaitReady(ctx, 2*time.Second)

	if !rig.Manager.IsAllReady() {
		t.Fatalf("expected manager ready, status=%+v", rig.Manager.Status())
	}
}
