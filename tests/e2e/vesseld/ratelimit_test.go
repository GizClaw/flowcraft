//go:build e2e

package vesseld_e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// rateLimitTemplate caps the mock LLM profile at exactly 1 request
// per minute. The token bucket admits the first call instantly,
// then forces the second to wait until the next minute window —
// which from the test's perspective is "indefinitely" within any
// reasonable test budget. We use that gap as the rate-limit
// signal: the first run completes; the second is still running
// after several seconds of polling.
//
// Without the wiring fix (#1) the limiter was never consulted, so
// both runs raced to completion in well under a second.
const rateLimitTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-rl
spec:
  control:
    socket: __SOCKET__
  shutdown:
    drainTimeout: 5s
  llmRateLimits:
    - llmProfile: mock-openai
      requestsPerMinute: 1
  logging:
    format: text
    level: info
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: LLMProfile
metadata:
  name: mock-openai
spec:
  provider: openai
  config:
    defaultModel: gpt-4o-mini
    baseURL: __OPENAI_URL__
  auth:
    apiKey:
      valueFrom:
        env: VESSELD_E2E_API_KEY
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: rl
spec:
  agents: [responder]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: responder
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
`

// TestE2E_RateLimit_Enforced fires two submits against a 1-rpm
// limit. The first run completes; the second blocks on Acquire
// until the next minute window. The test does not wait that long;
// instead it asserts that 3 seconds in, the second run is STILL
// running (or has been stuck on Acquire long enough to look that
// way from the registry's perspective).
func TestE2E_RateLimit_Enforced(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "ok"
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	cfg := strings.ReplaceAll(rateLimitTemplate, "__OPENAI_URL__", mock.URL())
	d := helpers.StartDaemon(t, bin, cfg)

	first := d.Submit(t, "rl", "responder", "first", nil)
	_ = d.WaitRun(t, first, 5*time.Second)

	second := d.Submit(t, "rl", "responder", "second", nil)
	// Give the second submit a moment to enter the engine and
	// hit Acquire.
	time.Sleep(2500 * time.Millisecond)

	var out map[string]any
	resp := d.GetJSON(t, "/v1/runs/"+second, &out)
	resp.Body.Close()
	state, _ := out["state"].(string)
	if state == "completed" {
		t.Fatalf("second submission completed within 2.5s — rate limit not enforced (1 rpm)")
	}
	// Useful diagnostic when the test fails for an unrelated
	// reason (e.g. registry GC dropping the entry).
	t.Logf("second run state after 2.5s = %q (expected running/blocked)", state)
}
