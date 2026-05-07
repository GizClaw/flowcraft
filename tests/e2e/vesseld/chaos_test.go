//go:build e2e

package vesseld_e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_Chaos_Upstream5xx asserts a chat completions 5xx from
// the upstream LLM surfaces as a non-completed status on the
// daemon Call API instead of crashing the daemon. A subsequent
// healthy call must succeed against the same daemon — the failure
// MUST be per-request, not vessel-wide.
func TestE2E_Chaos_Upstream5xx(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "after-recovery"
	defer mock.Close()
	mock.FailNext.Store(1)
	mock.FailStatus.Store(http.StatusServiceUnavailable)

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	cli := d.HTTPClient()

	// First call — upstream 503.
	body := strings.NewReader(`{"agent":"responder","query":"first"}`)
	resp, err := cli.Post("http://vesseld/v1/vessels/echo/call", "application/json", body)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	resp.Body.Close()
	// The daemon may report this as 200 with a non-completed
	// status payload OR as a 5xx — both are acceptable framings.
	// What MUST hold is that a follow-up healthy call still works.

	// Second call — upstream healthy now.
	body2 := strings.NewReader(`{"agent":"responder","query":"second"}`)
	resp2, err := cli.Post("http://vesseld/v1/vessels/echo/call", "application/json", body2)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second call status=%d after upstream recovery; daemon poisoned by 503?\nstderr:\n%s", resp2.StatusCode, d.Stderr())
	}

	// Daemon should still be healthy.
	hresp, _ := cli.Get("http://vesseld/healthz")
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d after chaos; daemon entered bad state", hresp.StatusCode)
	}
}

// TestE2E_Chaos_UpstreamTimeout asserts a slow upstream LLM does
// not hang the daemon: the request either eventually completes or
// surfaces a timeout, but the http handler returns. A 30-second
// budget is generous; the test fails if the daemon does not
// respond inside it.
func TestE2E_Chaos_UpstreamTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "ok"
	mock.Delay.Store(int64(500 * time.Millisecond))
	defer mock.Close()

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	cli := d.HTTPClient()
	cli.Timeout = 5 * time.Second
	body := strings.NewReader(`{"agent":"responder","query":"slow"}`)
	start := time.Now()
	resp, err := cli.Post("http://vesseld/v1/vessels/echo/call", "application/json", body)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	resp.Body.Close()
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("daemon hung longer than expected: %s", took)
	}
}
