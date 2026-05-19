package locomo_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall_v1"
)

// retryRunner is a minimal locomo.Runner that fails Save calls a configurable
// number of times with the supplied error class, then succeeds. It exists to
// pin down the single-shot NotAvailable retry contract in eval.ingestFlat —
// see the corresponding `attempt() / retry` block in eval.go.
type retryRunner struct {
	name        string
	failuresPer atomic.Int32 // remaining failures to emit before success
	failErr     error
	saveCalls   atomic.Int32
}

func (r *retryRunner) Name() string { return r.name }

func (r *retryRunner) Save(_ context.Context, _ recall.Scope, _ []llm.Message) (int, time.Duration, error) {
	r.saveCalls.Add(1)
	if r.failuresPer.Load() > 0 {
		r.failuresPer.Add(-1)
		return 0, 0, r.failErr
	}
	return 1, time.Millisecond, nil
}

func (r *retryRunner) Recall(_ context.Context, _ recall.Scope, _ string, _ int) ([]recall.Hit, time.Duration, error) {
	return nil, time.Millisecond, nil
}

func (r *retryRunner) Close() error { return nil }

// TestIngestRetriesNotAvailable verifies that a NotAvailable error on the
// first Save call triggers exactly one retry, and the second (successful)
// attempt is what counts in the report.
func TestIngestRetriesNotAvailable(t *testing.T) {
	r := &retryRunner{
		name:    "retry-stub",
		failErr: errdefs.NotAvailablef("simulated azure 404"),
	}
	r.failuresPer.Store(1)

	ds := dataset.Synthetic()
	// Single-conv slice keeps the assertion on saveCalls deterministic.
	ds.Conversations = ds.Conversations[:1]
	ds.Questions = ds.Questions[:1]

	report, err := locomo.Run(context.Background(), r, ds, locomo.Options{
		TopK:         5,
		UseExtractor: false,
		Concurrency:  1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := r.saveCalls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 Save calls (1 fail + 1 retry), got %d", got)
	}
	if report.Latency["save"].N == 0 {
		t.Fatalf("retry should land at least one successful save in latency, got %+v", report.Latency)
	}
}

// TestIngestDoesNotRetryValidation pins down that non-NotAvailable errors
// still fall through to the WARN-and-drop path — we don't want a runaway
// retry storm on real 400/422 misconfigurations.
func TestIngestDoesNotRetryValidation(t *testing.T) {
	r := &retryRunner{
		name:    "retry-stub-validation",
		failErr: errdefs.Validationf("simulated 400 bad request"),
	}
	r.failuresPer.Store(100) // unlimited fails — retry would surface as call=2

	ds := dataset.Synthetic()
	ds.Conversations = ds.Conversations[:1]
	ds.Questions = ds.Questions[:1]

	_, err := locomo.Run(context.Background(), r, ds, locomo.Options{
		TopK:         5,
		UseExtractor: false,
		Concurrency:  1,
	})
	if err != nil {
		t.Fatalf("Run should swallow per-batch errors, got: %v", err)
	}
	if got := r.saveCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Save call (no retry for Validation), got %d", got)
	}
}
