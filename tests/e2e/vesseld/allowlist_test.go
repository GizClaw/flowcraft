//go:build e2e

package vesseld_e2e

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// allowlistTwoAgentTemplate sets up two vessels off the same mock
// LLM:
//
//   - "plain"     hosts a regular agent with no Tools and no
//     dispatcher flag — should expose ZERO tool defs to the LLM.
//   - "kanboss"   hosts a dispatcher under a Kanban-enabled vessel
//     — should expose exactly kanban_submit and task_context.
//
// We then issue /call against each and inspect MockOpenAI's
// captured wire request to assert the projected `tools` array. The
// allow-list fix (#3) lives in graph-llm's buildToolDefinitions;
// without it both calls would see whatever happens to be registered
// (currently a superset of kanban tools across all dispatchers).
const allowlistTwoAgentTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-allow
spec:
  control:
    socket: __SOCKET__
  shutdown:
    drainTimeout: 3s
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
  name: plain
spec:
  agents: [pa]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: pa
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: kanboss
spec:
  agents: [boss, worker]
  kanban:
    maxProducerChain: 4
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: boss
spec:
  dispatcher: true
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: worker
spec:
  engine:
    ref: graph-llm
    config:
      llmProfile: mock-openai
`

// TestE2E_AllowList_PlainAgentExposesZeroTools asserts a plain
// agent (no Tools, not a dispatcher) results in an LLM request
// with no `tools` array. Pre-fix this could leak unrelated tools
// — kanban auto-injection wrote into the global registry and a
// future ToolPack registration would have been visible to every
// agent.
func TestE2E_AllowList_PlainAgentExposesZeroTools(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "fine"
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	cfg := strings.ReplaceAll(allowlistTwoAgentTemplate, "__OPENAI_URL__", mock.URL())
	d := helpers.StartDaemon(t, bin, cfg)

	body := map[string]any{"agent": "pa", "query": "hi"}
	resp := d.PostJSON(t, "/v1/vessels/plain/call", body, nil)
	resp.Body.Close()

	req, ok := mock.LastRequest()
	if !ok {
		t.Fatalf("mock saw no chat completion request; daemon did not reach LLM")
	}
	if len(req.Tools) != 0 {
		names := make([]string, 0, len(req.Tools))
		for _, tl := range req.Tools {
			if fn, ok := tl["function"].(map[string]any); ok {
				if n, ok := fn["name"].(string); ok {
					names = append(names, n)
				}
			}
		}
		t.Fatalf("plain agent's LLM request carried tools=%v; allow-list filter regressed", names)
	}
}

// TestE2E_AllowList_DispatcherExposesKanbanTools asserts a Kanban
// dispatcher's allow-list grows by exactly the auto-injected ids
// kanban_submit and task_context — and nothing else (since the
// agent itself declared no tools).
func TestE2E_AllowList_DispatcherExposesKanbanTools(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "fine"
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	cfg := strings.ReplaceAll(allowlistTwoAgentTemplate, "__OPENAI_URL__", mock.URL())
	d := helpers.StartDaemon(t, bin, cfg)

	mock.ResetRequests()
	body := map[string]any{"agent": "boss", "query": "delegate"}
	resp := d.PostJSON(t, "/v1/vessels/kanboss/call", body, nil)
	resp.Body.Close()

	req, ok := mock.LastRequest()
	if !ok {
		t.Fatalf("mock saw no chat completion request after dispatcher call")
	}
	got := map[string]bool{}
	for _, tl := range req.Tools {
		if fn, ok := tl["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				got[n] = true
			}
		}
	}
	for _, want := range []string{"kanban_submit", "task_context"} {
		if !got[want] {
			t.Errorf("dispatcher LLM request missing tool %q (got=%v)", want, keysOf(got))
		}
	}
	for n := range got {
		if n != "kanban_submit" && n != "task_context" {
			t.Errorf("dispatcher saw unexpected tool %q (allow-list leaked)", n)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
