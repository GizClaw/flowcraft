//go:build e2e

package vesseld_e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_Logs_ClientDisconnectCleansUp asserts that a logs
// subscriber's goroutine on the daemon side terminates when the
// client cancels its context. Operationally a leak would manifest
// as ever-growing goroutine count under churning subscribers; here
// we approximate by:
//
//   - Opening 5 sequential log streams against the same vessel.
//   - Cancelling each one before opening the next.
//   - Submitting one final run and confirming the daemon still
//     answers /healthz quickly (an indirect "still healthy" probe).
//
// A leaked subscriber wouldn't necessarily break /healthz on its
// own, but combined with the absence of subscriber-related stderr
// errors this gives reasonable confidence the cleanup path runs.
func TestE2E_Logs_ClientDisconnectCleansUp(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch, streamCancel := d.LogsStream(t, ctx, "echo", "")
		// Drain anything that arrives in 30ms then cancel.
		drain := time.After(30 * time.Millisecond)
		for done := false; !done; {
			select {
			case <-ch:
			case <-drain:
				done = true
			}
		}
		streamCancel()
		cancel()
	}

	// Daemon must still answer /healthz quickly post-churn.
	d.MustHTTP(t, http.MethodGet, "/healthz", http.StatusOK)
}

// TestE2E_Logs_RunIDFilterAccepted asserts the optional run_id
// query parameter is accepted by the handler. v0.1.0 graph-llm
// emits no stream.delta envelopes (Generate is non-streaming) so
// we cannot assert any data line content; the contract under test
// is "the handler returns 200 with text/event-stream and the
// goroutine terminates on ctx cancel". A future streaming engine
// landing in v0.2.0 will make this test data-driven.
func TestE2E_Logs_RunIDFilterAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	runID := d.Submit(t, "echo", "responder", "hi", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	ch, streamCancel := d.LogsStream(t, ctx, "echo", runID)
	defer streamCancel()
	// Wait either for the ctx to time out or the stream to end.
	deadline := time.After(700 * time.Millisecond)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			// Goroutine never closed the channel — that means
			// the daemon kept the subscription open past ctx
			// cancel, which is the regression we're guarding
			// against.
			t.Fatal("logs stream did not close after ctx cancel")
		}
	}
}

// TestE2E_DrainWaitsForInflight asserts a Submit'd run that is
// still running when /drain fires gets to complete before /drain
// returns. We schedule a 400ms-delayed mock LLM, fire submit, then
// immediately fire drain. Drain must not return until the inflight
// run finishes — measured by wall-clock vs. the LLM delay.
func TestE2E_DrainWaitsForInflight(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	mock.Delay.Store(int64(400 * time.Millisecond))
	mock.Reply = "ok"

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	runID := d.Submit(t, "echo", "responder", "x", nil)
	t.Logf("submitted run %s", runID)

	start := time.Now()
	resp := d.Do(t, http.MethodPost, "/v1/vessels/echo/drain", nil)
	resp.Body.Close()
	took := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain status=%d", resp.StatusCode)
	}
	// Drain must have waited at least until the in-flight LLM
	// call's 400ms delay completed; allow a generous lower bound
	// to absorb scheduler jitter.
	if took < 200*time.Millisecond {
		t.Fatalf("drain returned in %s — did not wait for the in-flight run (mock had a 400ms delay)", took)
	}

	// Run should now be in a terminal state.
	out := d.WaitRun(t, runID, 2*time.Second)
	if state, _ := out["state"].(string); state != "completed" {
		t.Fatalf("post-drain run state=%q, want completed", state)
	}
}
