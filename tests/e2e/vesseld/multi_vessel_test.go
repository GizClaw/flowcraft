//go:build e2e

package vesseld_e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// multiVesselTemplate wires two vessels off the same mock LLM so
// we can assert routing, list / phase / call / version semantics
// with a non-trivial fleet. A single Daemon doc; one shared
// LLMProfile; two Vessel + Agent pairs.
const multiVesselTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-multi
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
kind: Vessel
metadata:
  name: alpha
spec:
  agents: [responder-a]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: responder-a
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: beta
spec:
  agents: [responder-b]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: responder-b
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
`

func fillMultiConfig(openaiURL string) string {
	return strings.ReplaceAll(multiVesselTemplate, "__OPENAI_URL__", openaiURL)
}

// TestE2E_MultiVessel_RoutingAndAPI exercises the full HTTP API
// surface against a 2-vessel fleet:
//   - GET /v1/vessels lists both
//   - GET /v1/vessels/{id}/phase returns running for each
//   - POST /v1/vessels/{id}/call hits the right vessel/agent
//   - GET /v1/version returns a non-empty payload
func TestE2E_MultiVessel_RoutingAndAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "multi-ok"
	defer mock.Close()

	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillMultiConfig(mock.URL()))
	cli := d.HTTPClient()

	// /v1/version — daemon advertises something.
	resp, err := cli.Get("http://vesseld/v1/version")
	if err != nil {
		t.Fatalf("GET version: %v", err)
	}
	var ver map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&ver)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(ver) == 0 {
		t.Fatalf("version status=%d body=%v", resp.StatusCode, ver)
	}

	// /v1/vessels lists both.
	resp, err = cli.Get("http://vesseld/v1/vessels")
	if err != nil {
		t.Fatalf("GET vessels: %v", err)
	}
	var list []map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	got := map[string]bool{}
	for _, v := range list {
		got[v["name"]] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Fatalf("expected alpha+beta in list, got %+v", list)
	}

	// Per-vessel phase.
	for _, name := range []string{"alpha", "beta"} {
		resp, err := cli.Get("http://vesseld/v1/vessels/" + name + "/phase")
		if err != nil {
			t.Fatalf("GET phase %s: %v", name, err)
		}
		var p struct {
			Phase string `json:"phase"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&p)
		resp.Body.Close()
		if p.Phase != "running" {
			t.Fatalf("vessel %q phase=%q", name, p.Phase)
		}
	}

	// Routing: call to alpha hits responder-a, call to beta hits
	// responder-b. The mock answers both with "multi-ok"; we
	// assert each call reached the LLM by watching CallCount.
	startCalls := mock.CallCount.Load()
	for _, tc := range []struct{ vessel, agent string }{
		{"alpha", "responder-a"},
		{"beta", "responder-b"},
	} {
		body := strings.NewReader(`{"agent":"` + tc.agent + `","query":"hi"}`)
		resp, err := cli.Post("http://vesseld/v1/vessels/"+tc.vessel+"/call", "application/json", body)
		if err != nil {
			t.Fatalf("POST call %s: %v", tc.vessel, err)
		}
		var r struct{ Status string }
		_ = json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || r.Status != "completed" {
			t.Fatalf("call %s status=%d body.status=%q stderr:\n%s", tc.vessel, resp.StatusCode, r.Status, d.Stderr())
		}
	}
	if mock.CallCount.Load()-startCalls != 2 {
		t.Fatalf("expected 2 LLM calls (one per vessel), got %d", mock.CallCount.Load()-startCalls)
	}
}

// TestE2E_MultiVessel_AgentMismatch asserts that a call request
// referencing an agent that does not live in the named vessel
// errors out cleanly (404), not 500. This is the most common
// user-facing routing mistake.
func TestE2E_MultiVessel_AgentMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")
	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	d := helpers.StartDaemon(t, bin, fillMultiConfig(mock.URL()))

	body := strings.NewReader(`{"agent":"responder-b","query":"oops"}`)
	resp, err := d.HTTPClient().Post("http://vesseld/v1/vessels/alpha/call", "application/json", body)
	if err != nil {
		t.Fatalf("POST call: %v", err)
	}
	defer resp.Body.Close()
	// Either 404 (unknown agent) or 400 (validation) is acceptable;
	// 5xx is not. The exact mapping is the daemon's call.
	if resp.StatusCode >= 500 {
		t.Fatalf("status=%d, want 4xx for cross-vessel agent miss", resp.StatusCode)
	}
}
