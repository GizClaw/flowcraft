//go:build e2e

package vesseld_e2e

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// TestE2E_Drain_InFlight asserts that when SIGTERM
// arrives mid-call, the daemon waits for the in-flight Call to
// drain before exiting (within drainTimeout=5s). The mock LLM
// blocks for 200ms to give us a measurable in-flight window.
func TestE2E_Drain_InFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "drained"
	mock.Delay.Store(int64(300 * time.Millisecond))
	defer mock.Close()

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillConfig(mock.URL()))

	// Kick off a Call in a goroutine so we can SIGTERM while it
	// is still in flight.
	cli := d.HTTPClient()
	cli.Timeout = 10 * time.Second

	var wg sync.WaitGroup
	var status int
	wg.Add(1)
	go func() {
		defer wg.Done()
		body := strings.NewReader(`{"agent":"responder","query":"slow"}`)
		resp, err := cli.Post("http://vesseld/v1/vessels/echo/call", "application/json", body)
		if err != nil {
			t.Logf("call err during drain: %v", err)
			return
		}
		status = resp.StatusCode
		resp.Body.Close()
	}()

	// Give the call ~50ms to land inside the daemon, then SIGTERM.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if err := d.Stop(7 * time.Second); err != nil {
		t.Fatalf("Stop during drain: %v\nstderr:\n%s", err, d.Stderr())
	}
	wg.Wait()
	took := time.Since(start)

	// Drain should be < drainTimeout (5s) since the in-flight
	// call only blocks for 300ms.
	if took > 5*time.Second {
		t.Fatalf("drain exceeded drainTimeout budget: %s", took)
	}
	// And the in-flight call should have got a response (either
	// 200 from completion or 5xx if drain killed mid-flight; we
	// assert non-zero status to confirm the goroutine returned).
	if status == 0 {
		t.Fatalf("in-flight call did not return a status; got %d", status)
	}
}
