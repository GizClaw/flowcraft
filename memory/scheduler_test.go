package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory"
)

func TestMemoryJobStoreLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	store := memory.NewMemoryJobStore()

	jobID, err := store.Enqueue(ctx, memory.LifecycleJob{
		ID:          "job-1",
		OperationID: "op-1",
		Kind:        memory.LifecycleJobKindWriteChain,
		Scope:       testScope("conv-1"),
	})
	if err != nil {
		t.Fatalf("Enqueue error = %v", err)
	}
	if jobID != "job-1" {
		t.Fatalf("Enqueue jobID = %q, want job-1", jobID)
	}

	claimed, ok, err := store.Claim(ctx, "worker-a", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim worker-a error = %v", err)
	}
	if !ok || claimed.ID != jobID || claimed.Status != memory.LifecycleJobStatusRunning || claimed.Attempt != 1 || claimed.LeaseOwner != "worker-a" || claimed.LeaseExpiresAt.IsZero() {
		t.Fatalf("Claim worker-a = %+v ok=%v, want leased running job", claimed, ok)
	}

	if _, ok, err := store.Claim(ctx, "worker-b", 10*time.Millisecond); err != nil || ok {
		t.Fatalf("Claim worker-b before expiry ok=%v err=%v, want no job", ok, err)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats running error = %v", err)
	}
	if stats.Pending != 0 || stats.Running != 1 || stats.QueuedByKind[memory.LifecycleJobKindWriteChain] != 1 {
		t.Fatalf("Stats running = %+v, want one running write_chain", stats)
	}

	if err := store.Heartbeat(ctx, jobID, "worker-a", time.Hour); err != nil {
		t.Fatalf("Heartbeat owner error = %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	claimed, ok, err = store.Claim(ctx, "worker-b", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim after heartbeat error = %v", err)
	}
	if ok {
		t.Fatalf("Claim after heartbeat = %+v, want no claim while extended lease is active", claimed)
	}

	if err := store.Complete(ctx, jobID, "worker-b", memory.LifecycleJobResult{JobID: jobID}); err == nil {
		t.Fatal("Complete wrong owner error = nil, want lease owner validation")
	}
	if err := store.Complete(ctx, jobID, "worker-a", memory.LifecycleJobResult{
		JobID:      jobID,
		Completed:  true,
		Checkpoint: map[string]any{"cursor": "done"},
	}); err != nil {
		t.Fatalf("Complete owner error = %v", err)
	}

	stats, err = store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats completed error = %v", err)
	}
	if stats.Running != 0 || stats.Completed != 1 {
		t.Fatalf("Stats completed = %+v, want one completed", stats)
	}
	if stats.Attempts != 1 || stats.AttemptsByKind[memory.LifecycleJobKindWriteChain] != 1 || stats.CompletedByKind[memory.LifecycleJobKindWriteChain] != 1 {
		t.Fatalf("Stats completed metrics = %+v, want attempts/completed by kind", stats)
	}
}

func TestMemoryJobStoreFailCancelAndShutdown(t *testing.T) {
	ctx := context.Background()
	store := memory.NewMemoryJobStore()

	failedID, err := store.Enqueue(ctx, memory.LifecycleJob{
		OperationID: "op-fail",
		Kind:        memory.LifecycleJobKindReconcile,
		Scope:       testScope("conv-fail"),
	})
	if err != nil {
		t.Fatalf("Enqueue failed job error = %v", err)
	}
	claimed, ok, err := store.Claim(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("Claim failed job error = %v", err)
	}
	if !ok || claimed.ID != failedID {
		t.Fatalf("Claim failed job = %+v ok=%v, want %q", claimed, ok, failedID)
	}
	if err := store.Fail(ctx, failedID, "worker-a", errors.New("boom"), map[string]any{"stage": "reconcile"}); err != nil {
		t.Fatalf("Fail error = %v", err)
	}

	cancelledID, err := store.Enqueue(ctx, memory.LifecycleJob{
		OperationID: "op-cancel",
		Kind:        memory.LifecycleJobKindRebuild,
		Scope:       testScope("conv-cancel"),
	})
	if err != nil {
		t.Fatalf("Enqueue cancel job error = %v", err)
	}
	if err := store.Cancel(ctx, cancelledID, "operator cancelled"); err != nil {
		t.Fatalf("Cancel error = %v", err)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats terminal error = %v", err)
	}
	if stats.Failed != 1 || stats.Cancelled != 1 || stats.Pending != 0 || stats.Running != 0 {
		t.Fatalf("Stats terminal = %+v, want failed=1 cancelled=1", stats)
	}
	if stats.FailedByKind[memory.LifecycleJobKindReconcile] != 1 || stats.CancelledByKind[memory.LifecycleJobKindRebuild] != 1 {
		t.Fatalf("Stats terminal by kind = %+v, want failed reconcile and cancelled rebuild", stats)
	}

	if err := store.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown error = %v", err)
	}
	if _, err := store.Enqueue(ctx, memory.LifecycleJob{Kind: memory.LifecycleJobKindWriteChain}); err == nil {
		t.Fatal("Enqueue after shutdown error = nil, want not available")
	}
}

func TestMemoryJobStoreFailRetriesUntilMaxAttempts(t *testing.T) {
	ctx := context.Background()
	store := memory.NewMemoryJobStore()
	jobID, err := store.Enqueue(ctx, memory.LifecycleJob{
		OperationID: "op-retry",
		Kind:        memory.LifecycleJobKindReload,
		Scope:       testScope("conv-retry"),
		MaxAttempts: 2,
	})
	if err != nil {
		t.Fatalf("Enqueue retry job error = %v", err)
	}
	claimed, ok, err := store.Claim(ctx, "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("Claim first attempt error = %v", err)
	}
	if !ok || claimed.Attempt != 1 {
		t.Fatalf("Claim first attempt = %+v ok=%v, want attempt 1", claimed, ok)
	}
	if err := store.Fail(ctx, jobID, "worker-a", errors.New("first boom"), map[string]any{"cursor": "retry"}); err != nil {
		t.Fatalf("Fail first attempt error = %v", err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats after retryable fail error = %v", err)
	}
	if stats.Pending != 1 || stats.Failed != 0 || stats.Attempts != 1 || stats.AttemptsByKind[memory.LifecycleJobKindReload] != 1 {
		t.Fatalf("Stats after retryable fail = %+v, want pending retry with one attempt", stats)
	}
	claimed, ok, err = store.Claim(ctx, "worker-b", time.Minute)
	if err != nil {
		t.Fatalf("Claim retry attempt error = %v", err)
	}
	if !ok || claimed.ID != jobID || claimed.Attempt != 2 || claimed.Checkpoint["cursor"] != "retry" {
		t.Fatalf("Claim retry attempt = %+v ok=%v, want attempt 2 with checkpoint", claimed, ok)
	}
	if err := store.Fail(ctx, jobID, "worker-b", errors.New("second boom"), claimed.Checkpoint); err != nil {
		t.Fatalf("Fail second attempt error = %v", err)
	}
	stats, err = store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats after terminal fail error = %v", err)
	}
	if stats.Pending != 0 || stats.Failed != 1 || stats.Attempts != 2 || stats.FailedByKind[memory.LifecycleJobKindReload] != 1 {
		t.Fatalf("Stats after terminal fail = %+v, want failed after max attempts", stats)
	}
}
