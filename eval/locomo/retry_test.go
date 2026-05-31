package locomo_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo"
	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
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

func (r *retryRunner) Save(_ context.Context, _ runners.Scope, _ []llm.Message) (int, time.Duration, error) {
	r.saveCalls.Add(1)
	if r.failuresPer.Load() > 0 {
		r.failuresPer.Add(-1)
		return 0, 0, r.failErr
	}
	return 1, time.Millisecond, nil
}

func (r *retryRunner) Recall(_ context.Context, _ runners.Scope, _ string, _ int) ([]runners.RecallArtifact, time.Duration, error) {
	return nil, time.Millisecond, nil
}

func (r *retryRunner) Close() error { return nil }

func TestStartEventIncludesRetrievalBackend(t *testing.T) {
	r := &retryRunner{name: "flowcraft-recall-v1"}
	ds := dataset.Synthetic()
	ds.Conversations = ds.Conversations[:1]
	ds.Questions = ds.Questions[:1]

	var start locomo.Event
	_, err := locomo.Run(context.Background(), r, ds, locomo.Options{
		TopK:             5,
		UseExtractor:     false,
		Concurrency:      1,
		RetrievalBackend: "bbh",
		RunName:          "locomo-v1-bbh-test",
		Hook: func(_ context.Context, e locomo.Event) {
			if e.Kind == "start" {
				start = e
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(start.Title, "runner=flowcraft-recall-v1 retrieval=bbh") {
		t.Fatalf("start title = %q", start.Title)
	}
	if !strings.Contains(start.Title, "run=locomo-v1-bbh-test") {
		t.Fatalf("start title = %q", start.Title)
	}
	if !strings.Contains(start.Body, "source run=locomo-v1-bbh-test") {
		t.Fatalf("start body = %q", start.Body)
	}
	if got := start.Fields["retrieval_backend"]; got != "bbh" {
		t.Fatalf("retrieval_backend field = %q, want bbh", got)
	}
	if got := start.Fields["run"]; got != "locomo-v1-bbh-test" {
		t.Fatalf("run field = %q, want locomo-v1-bbh-test", got)
	}
	if start.Fields["pid"] == "" {
		t.Fatalf("pid field is empty: %+v", start.Fields)
	}
	if start.Fields["cwd"] == "" {
		t.Fatalf("cwd field is empty: %+v", start.Fields)
	}
}

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
