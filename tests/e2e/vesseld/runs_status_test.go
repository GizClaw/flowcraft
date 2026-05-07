//go:build e2e

package vesseld_e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_GetRunStatus_Lifecycle exercises the new /v1/runs/{run_id}
// endpoint that closes the loop on Submit (which only returns the
// run id; before this fix the result was unreachable).
//
// Flow:
//
//  1. Submit while the mock LLM holds the request for 200ms.
//  2. The first poll within 50ms must return "running" (or 200/404 –
//     whatever the daemon reports for an in-flight handle).
//  3. WaitRun polls until the registry transitions to a terminal
//     status; the payload MUST include status="completed" and the
//     vessel/agent ids the run was submitted under.
//
// This is the primary smoke test for fix #4 (Submit results not
// queryable). Without the registry the WaitRun call never sees a
// terminal state and the test times out.
func TestE2E_GetRunStatus_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "ok"
	mock.Delay.Store(int64(150 * time.Millisecond))
	defer mock.Close()

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	runID := d.Submit(t, "echo", "responder", "ping", nil)

	// Status while in-flight may be "running" (registry has the
	// handle) or 404 (registry is best-effort and the handle has
	// not been recorded yet). Both are acceptable; what we are
	// asserting is "no 5xx leak from the new endpoint".
	resp := d.Do(t, http.MethodGet, "/v1/runs/"+runID, nil)
	resp.Body.Close()
	if resp.StatusCode/100 == 5 {
		t.Fatalf("/v1/runs/{id} mid-flight returned %d (server error)", resp.StatusCode)
	}

	out := d.WaitRun(t, runID, 5*time.Second)
	if got, _ := out["state"].(string); got != "completed" {
		t.Fatalf("terminal state = %q, want completed; full payload: %+v\nstderr:\n%s", got, out, d.Stderr())
	}
	if got, _ := out["vessel"].(string); got != "echo" {
		t.Fatalf("run payload vessel = %q, want echo (payload=%+v)", got, out)
	}
	if got, _ := out["agent"].(string); got != "responder" {
		t.Fatalf("run payload agent = %q, want responder", got)
	}
}

// TestE2E_GetRunStatus_NotFound asserts the endpoint returns a
// proper 404 (not 500) when the run id is unknown. Regression guard
// for the registry's lookup error mapping.
func TestE2E_GetRunStatus_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	resp := d.Do(t, http.MethodGet, "/v1/runs/no-such-run", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run id status=%d, want 404", resp.StatusCode)
	}
}

// TestE2E_GetRunStatus_RetainedAfterCompletion asserts the registry
// still answers within its retention window (~hours by default; we
// only need ~seconds of evidence here). Without retention a Submit-
// based caller's "fire then poll" pattern races with completion.
func TestE2E_RunStatus_Retained(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "done"
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	runID := d.Submit(t, "echo", "responder", "x", nil)
	_ = d.WaitRun(t, runID, 5*time.Second)

	// Poll again after a small grace interval — must STILL be
	// queryable. If the registry GC sweeps too aggressively this
	// will surface as a 404 here.
	time.Sleep(200 * time.Millisecond)
	var out map[string]any
	resp := d.GetJSON(t, "/v1/runs/"+runID, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-completion lookup status=%d (registry too aggressive?)", resp.StatusCode)
	}
	if got, _ := out["state"].(string); got != "completed" {
		t.Fatalf("post-completion state=%q, want completed", got)
	}
}
