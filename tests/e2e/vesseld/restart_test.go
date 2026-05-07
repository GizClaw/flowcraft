//go:build e2e

package vesseld_e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// restartTemplate sets up a vessel whose liveness probe pings a
// permanently-broken LLM. The probe loop fails the configured
// threshold, the captain transitions to Failed, restartLoop
// re-Launches, probes fail again, and the cycle continues until
// the captain-level restartAttempts counter (shared across every
// restartLoop spawn) reaches MaxRestarts — at which point
// finalize() runs and the phase becomes Stopped.
//
// This pins three properties:
//   - The brittle vessel reaches PhaseStopped (proof that
//     probe-driven flap is bounded by MaxRestarts; pre-fix it was
//     an unbounded Running ↔ Failed loop because each restart
//     spawn reset its own attempt counter).
//   - The bystander vessel stays Running — a brittle vessel must
//     NOT take down siblings.
//   - The daemon process keeps answering /healthz.
const restartTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-restart
spec:
  control:
    socket: __SOCKET__
  shutdown:
    drainTimeout: 5s
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
kind: Probe
metadata:
  name: probe-llm
spec:
  ref: llm-reachable
  config:
    llmProfile: mock-openai
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: brittle
spec:
  agents: [responder]
  probes:
    liveness: [probe-llm]
    interval: 150ms
    timeout: 500ms
    failureThreshold: 2
  restart:
    mode: on_failure
    maxRestarts: 2
    backoffInit: 100ms
    backoffMax: 200ms
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
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: bystander
spec:
  agents: [other]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: other
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
`

// TestE2E_RestartLoop_BrittleVesselIsolation drives a probe cascade
// and asserts the captain-level MaxRestarts cap actually trips:
//   - the brittle vessel reaches PhaseStopped within budget,
//   - the bystander vessel stays running (brittle ≠ daemon-wide),
//   - /healthz still answers 200.
func TestE2E_RestartLoop_BrittleVesselIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	// Fail every chat completion call indefinitely; the probe's
	// LLMReachableProbe.Check issues a tiny Generate, sees the
	// 5xx, marks the round unhealthy.
	mock.FailNext.Store(1 << 30)
	mock.FailStatus.Store(http.StatusServiceUnavailable)

	bin := helpers.EnsureBinary(t)
	cfg := strings.ReplaceAll(restartTemplate, "__OPENAI_URL__", mock.URL())
	d := helpers.StartDaemon(t, bin, cfg)

	// Walk the brittle vessel to a terminal phase. Budget covers
	// (threshold=2 × interval=150ms) × (maxRestarts+1=3) plus
	// backoff (~300ms) and slack — well under 10s.
	// Accept only "stopped" — accepting "failed" would race with
	// the natural Failed → Pending → Running cycle the captain
	// goes through between attempts and exit early on the wrong
	// phase. With MaxRestarts=2, the third probe failure trips
	// the captain-level counter and runs finalize → Stopped.
	got := d.WaitPhase(t, "brittle", 15*time.Second, "stopped")
	if got != "stopped" {
		t.Fatalf("brittle vessel terminal phase = %q, want stopped (MaxRestarts must trip on probe flap)\nstderr:\n%s", got, d.Stderr())
	}

	// Bystander vessel must remain healthy — a daemon-wide
	// regression where one captain's exhaustion takes down the
	// fleet would surface here.
	if phase := d.Phase(t, "bystander"); phase != "running" {
		t.Errorf("bystander vessel phase = %q after brittle exhaustion; expected running", phase)
	}
	d.MustHTTP(t, http.MethodGet, "/healthz", http.StatusOK)
}
