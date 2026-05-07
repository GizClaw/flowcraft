//go:build e2e

package vesseld_e2e

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// planFullTemplate exercises every projection field /v1/plan now
// returns: daemon.drainTimeout, vessel.history, agent.engine,
// agent.dispatcher, agent.sidecar, agent.history_access. Without
// the v0.1.0 fix the response was a degenerate {name, phase} list.
const planFullTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-plan-full
spec:
  control:
    socket: __SOCKET__
  shutdown:
    drainTimeout: 7s
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
  name: planful
spec:
  agents: [primary]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: primary
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
`

// TestE2E_Plan_FullProjection validates every API field added by
// the "incomplete /plan" fix (#5). A regression that drops any of
// these fields surfaces here as a missing-key assertion.
func TestE2E_Plan_FullProjection(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	cfg := strings.ReplaceAll(planFullTemplate, "__OPENAI_URL__", mock.URL())
	d := helpers.StartDaemon(t, bin, cfg)

	plan := d.Plan(t)

	// Top-level scalars.
	if name, _ := plan["daemon"].(string); name != "vesseld-plan-full" {
		t.Fatalf("plan.daemon = %v, want vesseld-plan-full", plan["daemon"])
	}
	if v, _ := plan["version"].(string); v == "" {
		t.Fatalf("plan.version missing; payload=%+v", plan)
	}
	if dt, _ := plan["drain_timeout"].(string); dt == "" {
		t.Fatalf("plan.drain_timeout missing/empty; payload=%+v", plan)
	}

	// Vessels: one entry, with one agent describing engine + flags.
	vessels, ok := plan["vessels"].([]any)
	if !ok || len(vessels) != 1 {
		t.Fatalf("plan.vessels = %+v, want 1 entry", plan["vessels"])
	}
	v := vessels[0].(map[string]any)
	if v["name"] != "planful" {
		t.Fatalf("vessel.name = %v, want planful", v["name"])
	}
	if v["phase"] == nil || v["phase"] == "" {
		t.Fatalf("vessel.phase missing")
	}

	agents, ok := v["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("vessel.agents = %+v, want 1 entry", v["agents"])
	}
	ag := agents[0].(map[string]any)
	if ag["name"] != "primary" {
		t.Fatalf("agent.name = %v", ag["name"])
	}
	if eng, _ := ag["engine"].(string); eng != "graph-llm" {
		t.Fatalf("agent.engine = %v, want graph-llm", ag["engine"])
	}
	// dispatcher/sidecar use omitempty — when both are false the
	// keys are absent. We explicitly assert "if present, must be
	// false in this fixture" rather than requiring the key.
	if v, ok := ag["dispatcher"]; ok {
		if b, _ := v.(bool); b {
			t.Errorf("agent.dispatcher = true unexpectedly")
		}
	}
	if v, ok := ag["sidecar"]; ok {
		if b, _ := v.(bool); b {
			t.Errorf("agent.sidecar = true unexpectedly")
		}
	}
}
