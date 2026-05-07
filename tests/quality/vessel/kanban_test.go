package vesselquality

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"

	"github.com/GizClaw/flowcraft/tests/quality/vessel/fakellm"
)

// TestKanbanDispatchRoundTrip exercises the full agent-as-tool
// path through real LLM tool_call replies (rather than directly
// invoking the kanban_submit tool as vessel/kanban_test.go does):
//
//  1. boss receives a user message → fake LLM emits a kanban_submit
//     tool_call targeting "worker".
//  2. The auto-injected vesselSubmitTool dispatches the task; the
//     same engine loop appends the tool result and calls Generate
//     again (turn 2). Boss's fake replies with placeholder text
//     since worker has not run yet.
//  3. The kanban runtime spawns worker; worker's fake LLM returns
//     a final text. The callback bridge then re-Submits boss with
//     a "[Task Callback] ..." user message.
//  4. Boss's fake LLM (turn 3) consumes the callback and produces
//     the final answer.
//
// The test asserts the callback message reached boss's history and
// that the final assistant message references the worker output.
func TestKanbanDispatchRoundTrip(t *testing.T) {
	t.Parallel()

	bossFake := fakellm.New([]fakellm.Step{
		{ToolCalls: []fakellm.Tool{{
			Name: "kanban_submit",
			Args: `{"target_agent_id":"worker","query":"please summarise","user_query":"please summarise","dispatch_note":"test"}`,
		}}},
		{Text: "dispatched, waiting for worker"},
	}, fakellm.WithRepeatLast())
	workerFake := fakellm.New([]fakellm.Step{
		{Text: "worker-result-payload"},
	}, fakellm.WithRepeatLast())

	vs := spec.Spec{
		ID: "v-kanban",
		Agents: []spec.Agent{
			{Name: "boss", Dispatcher: true, HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "worker", HistoryAccess: spec.HistoryAccessReadWrite},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 50},
		Kanban:  &spec.Kanban{MaxPendingTasks: 8, MaxProducerChain: 3},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{
			"boss":   bossFake,
			"worker": workerFake,
		}, 6)),
	)

	res, err := c.Call(context.Background(), "boss", agent.Request{
		ContextID: "conv-k",
		Message:   model.NewTextMessage(model.RoleUser, "kick"),
	})
	if err != nil {
		t.Fatalf("Call boss: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("boss status = %s", res.Status)
	}

	// Wait for the worker callback bridge to re-trigger boss with
	// "[Task Callback] ...". The bridge runs asynchronously so we
	// poll boss.Calls() — the third recorded boss-LLM call's last
	// user message MUST start with "[Task Callback]".
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		calls := bossFake.Calls()
		if len(calls) >= 3 {
			last := calls[2].Messages
			if len(last) > 0 {
				tail := last[len(last)-1].Content()
				if strings.HasPrefix(tail, "[Task Callback]") {
					if !strings.Contains(tail, "worker-result-payload") {
						t.Fatalf("callback missing worker payload: %q", tail)
					}
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("boss never received [Task Callback] turn; calls=%d", len(bossFake.Calls()))
}

// TestKanbanWorkerErrorSurfacesAsCallback asserts that when a
// worker's LLM errors out, the callback bridge still fires and
// boss receives a [Task Callback] turn (rather than the failure
// silently disappearing).
func TestKanbanWorkerErrorSurfacesAsCallback(t *testing.T) {
	t.Parallel()
	bossFake := fakellm.New([]fakellm.Step{
		{ToolCalls: []fakellm.Tool{{
			Name: "kanban_submit",
			Args: `{"target_agent_id":"worker","query":"q","user_query":"q","dispatch_note":""}`,
		}}},
		{Text: "submitted"},
	}, fakellm.WithRepeatLast())

	provErr := errFakeWorker
	workerFake := fakellm.New([]fakellm.Step{
		{Err: provErr},
	}, fakellm.WithRepeatLast())

	vs := spec.Spec{
		ID: "v-kanban-err",
		Agents: []spec.Agent{
			{Name: "boss", Dispatcher: true, HistoryAccess: spec.HistoryAccessReadWrite},
			{Name: "worker", HistoryAccess: spec.HistoryAccessReadWrite},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 20},
		Kanban:  &spec.Kanban{MaxPendingTasks: 4, MaxProducerChain: 2},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{
			"boss":   bossFake,
			"worker": workerFake,
		}, 6)),
	)
	if _, err := c.Call(context.Background(), "boss", agent.Request{
		ContextID: "conv-err",
		Message:   model.NewTextMessage(model.RoleUser, "kick"),
	}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range bossFake.Calls()[2:] { // entries past the first two
			last := call.Messages
			if len(last) > 0 && strings.HasPrefix(last[len(last)-1].Content(), "[Task Callback]") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("boss never observed callback for failed worker; calls=%d", len(bossFake.Calls()))
}
