//go:build e2e

package vesseld_e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/tests/e2e/vesseld/helpers"
)

// kanbanTwoAgentTemplate exposes a vessel with one dispatcher
// (boss) and one worker. The mock LLM is fed scripted replies so
// the boss's first turn emits a kanban_submit tool_call, the
// worker turn returns a plain-text result, and the boss's second
// turn (after the synchronous tool_result) finalises with a stop
// message. Then the async kanban callback re-dispatches the boss
// with the [Task Callback] user message and we observe a third
// boss turn.
const kanbanTwoAgentTemplate = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-kb
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
  name: kb
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

// TestE2E_Kanban_DispatcherCallback runs the full dispatcher path:
// boss issues kanban_submit → worker runs → callback comes back to
// boss. The mock's CallCount captures every LLM hit; we assert
// there were AT LEAST 3 calls (boss-turn1, worker, boss-turn2)
// proving:
//
//  1. The dispatcher's allow-list let the LLM see kanban_submit.
//  2. The Captain's executor really dispatched the worker.
//  3. graph-llm fed back the tool_result so boss could finalise.
//
// We do not strictly assert the callback turn (which depends on
// the bus async timing); WaitStderr on the kanban-task-completed
// log line is the closest non-flaky callback signal.
func TestE2E_Kanban_DispatcherCallback(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	defer mock.Close()
	// Boss turn 1: emit kanban_submit.
	mock.QueueToolCall("kanban_submit", `{"target_agent_id":"worker","query":"do the thing"}`)
	// Worker turn: plain text completion.
	mock.QueueText("worker finished")
	// Boss turn 2 (after seeing the kanban_submit tool_result): finalise.
	mock.QueueText("queued; awaiting callback")
	// Boss turn 3 (callback re-dispatch, may or may not run by the
	// time /call returns; queueing here means the mock has it
	// ready when the callback re-enters the LLM).
	mock.QueueText("all done")

	bin := helpers.EnsureBinary(t)
	cfg := strings.ReplaceAll(kanbanTwoAgentTemplate, "__OPENAI_URL__", mock.URL())
	d := helpers.StartDaemon(t, bin, cfg)

	body := map[string]any{"agent": "boss", "query": "delegate"}
	var call struct {
		Status string `json:"status"`
	}
	resp := d.PostJSON(t, "/v1/vessels/kb/call", body, &call)
	resp.Body.Close()
	if call.Status != "completed" {
		t.Fatalf("boss call status=%q, want completed; stderr:\n%s", call.Status, d.Stderr())
	}

	// Verify the worker also ran. We give the kanban callback
	// loop ~500ms to see boss-turn1 + worker + boss-turn2 land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mock.CallCount.Load() >= 3 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := mock.CallCount.Load(); got < 3 {
		t.Fatalf("LLM call count = %d, want >= 3 (boss turn1 + worker + boss turn2); stderr:\n%s", got, d.Stderr())
	}

	// At least one of the captured requests MUST be the worker's
	// turn — its system/user message is "do the thing". We grep
	// across all captured requests' message blobs.
	// Worker turn carries content as structured parts ([{text:...}]),
	// not a flat string. We grep the raw JSON wire bytes for the
	// distinctive task instruction; that avoids re-implementing
	// OpenAI's content-part traversal here.
	saw := false
	for _, req := range mock.AllRequests() {
		if strings.Contains(string(req.Raw), "do the thing") {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("worker LLM turn never received the dispatched query; mock saw %d requests", len(mock.AllRequests()))
	}
}

// TestE2E_Kanban_NonDispatcherDoesNotSeeTools asserts an agent
// without dispatcher=true (the worker in our fixture, when
// directly /called) does NOT see kanban_submit in its tool defs.
// This is a per-agent allow-list assertion that lives most cleanly
// alongside the kanban setup.
func TestE2E_Kanban_NonDispatcherNoTools(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	t.Setenv("VESSELD_E2E_API_KEY", "sk-e2e-fake")

	mock := helpers.NewMockOpenAI()
	mock.Reply = "ok"
	defer mock.Close()
	bin := helpers.EnsureBinary(t)
	cfg := strings.ReplaceAll(kanbanTwoAgentTemplate, "__OPENAI_URL__", mock.URL())
	d := helpers.StartDaemon(t, bin, cfg)

	mock.ResetRequests()
	body := map[string]any{"agent": "worker", "query": "x"}
	resp := d.PostJSON(t, "/v1/vessels/kb/call", body, nil)
	resp.Body.Close()

	req, ok := mock.LastRequest()
	if !ok {
		t.Fatalf("mock saw no requests")
	}
	for _, tl := range req.Tools {
		if fn, ok := tl["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && (n == "kanban_submit" || n == "task_context") {
				t.Errorf("worker (non-dispatcher) saw tool %q in its allow-list; should be empty", n)
			}
		}
	}
}
