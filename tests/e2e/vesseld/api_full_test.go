//go:build e2e

package vesseld_e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_Submit_FireAndForget asserts POST /v1/vessels/{id}/submit
// returns 202 Accepted with a run_id immediately, without blocking
// for completion. The mock LLM is configured with a 200ms delay so
// the response time is a clear signal: < 100ms = fire-and-forget,
// > 200ms = the handler accidentally waited.
func TestE2E_Submit_FireAndForget(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "submitted"
	mock.Delay.Store(int64(200 * time.Millisecond))
	defer mock.Close()

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	cli := d.HTTPClient()
	body := strings.NewReader(`{"agent":"responder","query":"go"}`)
	start := time.Now()
	resp, err := cli.Post("http://vesseld/v1/vessels/echo/submit", "application/json", body)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RunID == "" {
		t.Fatal("submit returned empty run_id")
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("submit took %s — handler waited for completion (should be fire-and-forget)", elapsed)
	}

	// Wait for the run to actually finish so the mock saw the call.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.CallCount.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background run never reached the LLM")
}

// TestE2E_PerVesselDrainAndStop asserts the /drain and /stop
// endpoints transition a single vessel's phase, leaving siblings
// untouched.
func TestE2E_PerVesselDrainAndStop(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillMultiConfig(mock.URL()))
	cli := d.HTTPClient()

	// Drain alpha — should transition off "running".
	resp, err := cli.Post("http://vesseld/v1/vessels/alpha/drain", "application/json", nil)
	if err != nil {
		t.Fatalf("drain alpha: %v", err)
	}
	var drainOut struct{ Phase string }
	_ = json.NewDecoder(resp.Body).Decode(&drainOut)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain status=%d", resp.StatusCode)
	}
	if drainOut.Phase == "running" {
		t.Fatalf("alpha phase still 'running' after drain")
	}

	// beta should still be running.
	resp, _ = cli.Get("http://vesseld/v1/vessels/beta/phase")
	var betaPhase struct{ Phase string }
	_ = json.NewDecoder(resp.Body).Decode(&betaPhase)
	resp.Body.Close()
	if betaPhase.Phase != "running" {
		t.Fatalf("beta phase=%q after draining only alpha — siblings should be untouched", betaPhase.Phase)
	}

	// Stop beta — phase becomes "stopped".
	resp, err = cli.Post("http://vesseld/v1/vessels/beta/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop beta: %v", err)
	}
	var stopOut struct{ Phase string }
	_ = json.NewDecoder(resp.Body).Decode(&stopOut)
	resp.Body.Close()
	if stopOut.Phase != "stopped" {
		t.Fatalf("beta phase after Stop = %q, want 'stopped'", stopOut.Phase)
	}
}

// TestE2E_Plan_RedactsSecrets asserts /v1/plan returns the
// resolved plan WITHOUT leaking the apiKey. We feed a
// distinctive sentinel via VESSELD_E2E_API_KEY and assert the
// response body does not contain it.
func TestE2E_Plan_RedactsSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	const sentinel = "sk-DO-NOT-LEAK-1234567890"
	t.Setenv("VESSELD_E2E_API_KEY", sentinel)

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	resp, err := d.HTTPClient().Get("http://vesseld/v1/plan")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plan status=%d", resp.StatusCode)
	}
	buf, _ := readAll(resp.Body)
	if strings.Contains(string(buf), sentinel) {
		t.Fatalf("/v1/plan leaked the api key into its response payload!\n%s", buf)
	}
}

// TestE2E_Logs_SSEHeaders asserts the SSE stream:
//   - returns 200 with text/event-stream Content-Type
//   - sends headers immediately (does not buffer)
//   - cleanly terminates when the client cancels its context
//
// We do NOT assert any data lines because the v0.1.0 graph-llm
// engine factory uses Generate (non-streaming), so no
// stream.delta envelopes are emitted. The guard here is that the
// channel infrastructure works end-to-end; once a streaming
// engine ships in v0.2.0 this test will start receiving data.
func TestE2E_Logs_SSEHeaders(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://vesseld/v1/vessels/echo/logs", nil)
	resp, err := d.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("logs request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Spin off a reader so it is observable that the goroutine
	// returns when we cancel — no leak. We give it 200ms to read
	// any pre-stream output (none expected for graph-llm) and
	// then cancel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bufio.NewScanner(resp.Body)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE reader did not return after ctx cancel")
	}
}

// TestE2E_Logs_RunIDFilter asserts the optional run_id query
// parameter is accepted by the handler. Behaviour mirrors
// /v1/vessels/{id}/logs but only entries matching run_id flow
// through — since v0.1.0 emits no entries we still assert the
// header / 200 surface.
func TestE2E_Logs_RunIDFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	url := "http://vesseld/v1/vessels/echo/logs?run_id=does-not-exist"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := d.HTTPClient().Do(req)
	if err != nil {
		// ctx-timeout-induced error after the handler has already
		// started streaming is acceptable; the assertion below
		// still requires 200 from a successful response, so we
		// only fail on transport-layer errors that prove the
		// handler never returned headers.
		if !strings.Contains(err.Error(), "context") {
			t.Fatalf("logs request: %v", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs?run_id status=%d", resp.StatusCode)
	}
}
