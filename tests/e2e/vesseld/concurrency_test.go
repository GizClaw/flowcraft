//go:build e2e

package vesseld_e2e

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// concurrencyGateTemplate sets a daemon-wide maxConcurrentRuns
// budget of 1. With three submits arriving simultaneously and the
// mock holding each for 200ms, a working gate yields a total
// elapsed time of ~600ms (1+1+1 serially); a missing gate would
// finish in ~200ms (all three in flight at once).
const concurrencyGateTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-cc
spec:
  control:
    socket: __SOCKET__
  shutdown:
    drainTimeout: 5s
  resources:
    maxConcurrentRuns: 1
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
  name: cc
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

// TestE2E_DaemonConcurrencyGate asserts the global concurrent-runs
// gate (Daemon.spec.resources.maxConcurrentRuns) actually
// serialises submissions. Three runs against a 1-slot gate with
// 200ms each must take ≥ ~500ms wall clock (allow scheduler
// jitter); a missing gate finishes well under 300ms.
func TestE2E_DaemonConcurrencyGate(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "ok"
	mock.Delay.Store(int64(200 * time.Millisecond))
	defer mock.Close()

	bin := helpers.EnsureBinary(t)
	cfg := strings.ReplaceAll(concurrencyGateTemplate, "__OPENAI_URL__", mock.URL())
	d := helpers.StartDaemon(t, bin, cfg)

	const n = 3
	runIDs := make([]string, n)
	start := time.Now()
	var submitWG sync.WaitGroup
	submitWG.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer submitWG.Done()
			runIDs[i] = d.Submit(t, "cc", "responder", "x", nil)
		}()
	}
	submitWG.Wait()

	for _, rid := range runIDs {
		_ = d.WaitRun(t, rid, 5*time.Second)
	}
	elapsed := time.Since(start)
	if elapsed < 500*time.Millisecond {
		t.Fatalf("3 runs against a 1-slot gate finished in %s — gate not enforced", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("3 runs took %s — that's surprisingly slow even for a serialised path", elapsed)
	}
}
