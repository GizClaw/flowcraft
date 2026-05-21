package recall

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/asyncsemantic"
)

func TestAsyncSemanticQueueStats_RequiresScope(t *testing.T) {
	mem, err := New(WithAsyncSemanticQueue(asyncsemantic.New()))
	if err != nil {
		t.Fatal(err)
	}
	obs, ok := mem.(AsyncSemanticQueueObserver)
	if !ok {
		t.Fatal("observer missing")
	}
	_, err = obs.AsyncSemanticQueueStats(context.Background(), Scope{})
	if err == nil {
		t.Fatal("zero scope must fail")
	}
}

func TestAsyncSemanticQueueStats_ReportsBacklogAndDeadLetter(t *testing.T) {
	queue := asyncsemantic.New()
	ctx := context.Background()
	scope := asyncTestScope()

	mem, err := New(
		WithAsyncSemanticQueue(queue),
		withCompiler(testSemanticIngestor()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	obs, ok := mem.(AsyncSemanticQueueObserver)
	if !ok {
		t.Fatal("Memory must implement AsyncSemanticQueueObserver")
	}

	_, err = mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	stats, err := obs.AsyncSemanticQueueStats(ctx, scope)
	if err != nil {
		t.Fatalf("AsyncSemanticQueueStats: %v", err)
	}
	if stats.Pending != 1 {
		t.Fatalf("Pending = %d, want 1", stats.Pending)
	}

	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	_, _ = proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{
		Limit: 1,
		Scope: scope,
		Now:   time.Now().Add(25 * time.Hour),
	})

	stats, err = obs.AsyncSemanticQueueStats(ctx, scope)
	if err != nil {
		t.Fatalf("AsyncSemanticQueueStats after drain: %v", err)
	}
	if stats.Completed != 1 || stats.Pending != 0 {
		t.Fatalf("stats after drain = %+v, want Completed=1 Pending=0", stats)
	}

	// Permanent fail -> dead-letter bucket.
	_, _ = mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t2", Text: "gone"}},
	})
	jobs, err := queue.Claim(ctx, port.AsyncSemanticClaimOptions{Max: 1, Now: time.Now()})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %v jobs=%d", err, len(jobs))
	}
	_ = queue.Fail(ctx, jobs[0].RequestID, port.AsyncSemanticFailure{
		ErrClass: diagnostic.ErrClassPermanent,
		Err:      "episode missing",
	})
	stats, err = obs.AsyncSemanticQueueStats(ctx, scope)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.DeadLetter != 1 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want DeadLetter=1 Failed=1", stats)
	}
}
