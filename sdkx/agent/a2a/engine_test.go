package a2a_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/agent/a2a"
)

func textParts(texts ...string) []any {
	parts := make([]any, 0, len(texts))
	for _, t := range texts {
		parts = append(parts, map[string]any{"text": t})
	}
	return parts
}

// TestPollCompletion exercises the non-streaming path: message/send returns
// a submitted task and tasks/get polling reaches a completed task whose
// history is transcribed onto the board.
func TestPollCompletion(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(json.RawMessage) (any, *rpcErr) {
		return map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_SUBMITTED", nil, nil)}, nil
	}
	gets := 0
	f.getFn = func(json.RawMessage) (any, *rpcErr) {
		gets++
		if gets == 1 {
			return taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil), nil
		}
		return taskV1("t1", "c1", "TASK_STATE_COMPLETED",
			[]any{msgV1("m1", "ROLE_AGENT", textParts("hello from remote"))}, nil), nil
	}

	eng := newTestEngine(t, f, false, a2a.WithStreamMode(a2a.StreamModeOff), a2a.WithPollInterval(time.Millisecond))
	host := agenttest.NewMockHost()
	res := runTurn(t, eng, "hi there", agent.WithHost(host))

	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed (err=%v)", res.Status, res.Err)
	}
	if got := len(res.Messages); got != 1 {
		t.Fatalf("assistant messages = %d, want 1", got)
	}
	if got := res.Messages[0].Content.Parts[0]; !textEquals(got, "hello from remote") {
		t.Errorf("assistant part = %#v, want text hello from remote", got)
	}
	if got := res.LastBoard.GetVarString("a2a.task_id"); got != "t1" {
		t.Errorf("board a2a.task_id = %q, want t1", got)
	}
	if gets < 2 {
		t.Errorf("tasks/get calls = %d, want at least 2", gets)
	}
}

// TestDirectMessageReply verifies a message/send that returns a direct
// Message (no task tracking) completes immediately.
func TestDirectMessageReply(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(json.RawMessage) (any, *rpcErr) {
		return map[string]any{"message": msgV1("m1", "ROLE_AGENT", textParts("direct reply"))}, nil
	}
	eng := newTestEngine(t, f, false, a2a.WithStreamMode(a2a.StreamModeOff))
	res := runTurn(t, eng, "ping")

	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	if len(res.Messages) != 1 || !textEquals(res.Messages[0].Content.Parts[0], "direct reply") {
		t.Errorf("messages = %#v, want one direct reply", res.Messages)
	}
}

// TestStreaming verifies the SSE path: task + status updates + a streamed
// agent message are transcribed, and the run ends at the completed status.
func TestStreaming(t *testing.T) {
	f := newFakeA2A(t)
	f.streamFn = func(json.RawMessage) []json.RawMessage {
		events := []any{
			map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_SUBMITTED", nil, nil)},
			map[string]any{"statusUpdate": map[string]any{
				"taskId": "t1", "contextId": "c1",
				"status": map[string]any{"state": "TASK_STATE_WORKING"}}},
			map[string]any{"message": msgV1("m1", "ROLE_AGENT", textParts("streamed hello"))},
			map[string]any{"statusUpdate": map[string]any{
				"taskId": "t1", "contextId": "c1",
				"status": map[string]any{"state": "TASK_STATE_COMPLETED"}}},
		}
		out := make([]json.RawMessage, 0, len(events))
		for _, ev := range events {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "r1", "result": ev})
			out = append(out, raw)
		}
		return out
	}

	eng := newTestEngine(t, f, true, a2a.WithStreamMode(a2a.StreamModeOn))
	host := agenttest.NewMockHost()
	res := runTurn(t, eng, "stream please", agent.WithHost(host))

	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	if len(res.Messages) != 1 || !textEquals(res.Messages[0].Content.Parts[0], "streamed hello") {
		t.Errorf("messages = %#v, want streamed hello", res.Messages)
	}
	if got := res.LastBoard.GetVarString("a2a.task_id"); got != "t1" {
		t.Errorf("board a2a.task_id = %q, want t1", got)
	}
	if sends, _, _, _ := f.counts(); sends != 0 {
		t.Errorf("message/send calls = %d, want 0 (streaming used)", sends)
	}
}

// TestStreamingFallback verifies the SDK client falls back to message/send
// when the card does not advertise streaming even if the mode is "on".
func TestStreamingFallback(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(json.RawMessage) (any, *rpcErr) {
		return map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_COMPLETED",
			[]any{msgV1("m1", "ROLE_AGENT", textParts("fell back"))}, nil)}, nil
	}
	eng := newTestEngine(t, f, false, a2a.WithStreamMode(a2a.StreamModeOn))
	res := runTurn(t, eng, "hi")
	if res.Status != agent.StatusCompleted || len(res.Messages) != 1 {
		t.Fatalf("status=%q messages=%d, want completed with one message", res.Status, len(res.Messages))
	}
}

// TestArtifactAggregation verifies artifact chunks are assembled by id and
// emitted as an assistant message on completion.
func TestArtifactAggregation(t *testing.T) {
	f := newFakeA2A(t)
	f.streamFn = func(json.RawMessage) []json.RawMessage {
		chunk := func(append_, lastChunk bool, text string) any {
			return map[string]any{"artifactUpdate": map[string]any{
				"taskId": "t1", "contextId": "c1",
				"artifact": map[string]any{
					"artifactId": "a1",
					"parts":      []any{map[string]any{"text": text}},
				},
				"append": append_, "lastChunk": lastChunk,
			}}
		}
		events := []any{
			map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil)},
			chunk(false, false, "part-one-"),
			chunk(true, true, "part-two"),
			map[string]any{"statusUpdate": map[string]any{
				"taskId": "t1", "contextId": "c1",
				"status": map[string]any{"state": "TASK_STATE_COMPLETED"}}},
		}
		out := make([]json.RawMessage, 0, len(events))
		for _, ev := range events {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "r1", "result": ev})
			out = append(out, raw)
		}
		return out
	}
	eng := newTestEngine(t, f, true, a2a.WithStreamMode(a2a.StreamModeOn))
	res := runTurn(t, eng, "build it")

	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("messages = %#v, want one assembled artifact message", res.Messages)
	}
	if got := strings.Join(messageTexts(res.Messages), ""); got != "part-one-part-two" {
		t.Errorf("assembled artifact text = %q, want part-one-part-two", got)
	}
}

// TestInputRequired verifies HITL bridging: the remote question is appended,
// the host is asked, and the reply is sent back to the same task.
func TestInputRequired(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(json.RawMessage) (any, *rpcErr) {
		return map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil)}, nil
	}
	gets := 0
	f.getFn = func(params json.RawMessage) (any, *rpcErr) {
		gets++
		switch gets {
		case 1:
			// The remote question rides on status.message, the canonical
			// A2A place for an input-required prompt.
			task := taskV1("t1", "c1", "TASK_STATE_INPUT_REQUIRED", nil, nil)
			task["status"].(map[string]any)["message"] =
				msgV1("q1", "ROLE_AGENT", textParts("what is your name?"))
			return task, nil
		case 2:
			return taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil), nil
		default:
			return taskV1("t1", "c1", "TASK_STATE_COMPLETED", []any{
				msgV1("m1", "ROLE_AGENT", textParts("nice to meet you")),
			}, nil), nil
		}
	}

	eng := newTestEngine(t, f, false, a2a.WithStreamMode(a2a.StreamModeOff), a2a.WithPollInterval(time.Millisecond))
	host := agenttest.NewMockHost()
	host.SetUserReply(&agent.UserReply{Parts: []message.Part{message.TextPart{Text: "alice"}}})
	res := runTurn(t, eng, "hello", agent.WithHost(host))

	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	prompts := host.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	if !textEquals(prompts[0].Parts[0], "what is your name?") {
		t.Errorf("prompt part = %#v, want the remote question", prompts[0].Parts[0])
	}
	// Assistant transcript: the remote question, then the final answer.
	// (The user reply "alice" is a user turn on the board, not in
	// Result.Messages.)
	// Result.Messages is the trailing assistant block only (the harness
	// stops at the first user turn), so it carries the final answer.
	if len(res.Messages) != 1 || !textEquals(res.Messages[0].Content.Parts[0], "nice to meet you") {
		t.Errorf("Result.Messages = %#v, want the final answer", res.Messages)
	}
	// The full transcript lives on the board: user hello, assistant
	// question, user reply, assistant answer.
	boardTexts := messageTexts(res.LastBoard.Channel(agent.MainChannel))
	want := []string{"hello", "what is your name?", "alice", "nice to meet you"}
	if len(boardTexts) != len(want) {
		t.Fatalf("board transcript = %v, want %v", boardTexts, want)
	}
	for i := range want {
		if boardTexts[i] != want[i] {
			t.Errorf("board transcript[%d] = %q, want %q", i, boardTexts[i], want[i])
		}
	}
}

// TestInterruptCancelsTask verifies a cooperative interrupt cancels the
// remote task and surfaces as errdefs.IsInterrupted.
func TestInterruptCancelsTask(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(json.RawMessage) (any, *rpcErr) {
		return map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil)}, nil
	}
	f.getFn = func(json.RawMessage) (any, *rpcErr) {
		return taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil), nil
	}
	f.cancelFn = func(json.RawMessage) (any, *rpcErr) {
		return taskV1("t1", "c1", "TASK_STATE_CANCELED", nil, nil), nil
	}

	eng := newTestEngine(t, f, false, a2a.WithStreamMode(a2a.StreamModeOff), a2a.WithPollInterval(time.Millisecond))
	host := agenttest.NewMockHost()
	host.Interrupt(agent.CauseUserInput, "barge-in")

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "test-agent"}, eng, agent.Request{
		Message: message.Message{Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "stop"}}}},
	}, agent.WithHost(host))
	if err != nil {
		t.Fatalf("agent.Execute returned infrastructure error: %v", err)
	}
	if res.Status != agent.StatusInterrupted || !errdefs.IsInterrupted(res.Err) {
		t.Fatalf("status = %q err = %v, want interrupted", res.Status, res.Err)
	}
	if _, _, cancels, _ := f.counts(); cancels != 1 {
		t.Errorf("tasks/cancel calls = %d, want 1", cancels)
	}
}

// TestContextCancel verifies a cancelled context surfaces as ctx.Err().
func TestContextCancel(t *testing.T) {
	f := newFakeA2A(t)
	f.getFn = func(json.RawMessage) (any, *rpcErr) {
		return taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil), nil
	}
	eng := newTestEngine(t, f, false, a2a.WithStreamMode(a2a.StreamModeOff), a2a.WithPollInterval(time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := agent.Execute(ctx, agent.Agent{ID: "test-agent"}, eng, agent.Request{
		Message: message.Message{Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}}},
	}, agent.WithHost(agenttest.NewMockHost()))
	if err != nil {
		t.Fatalf("agent.Execute returned infrastructure error: %v", err)
	}
	if res.Status != agent.StatusCanceled || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("status = %q err = %v, want canceled with context.Canceled", res.Status, res.Err)
	}
}

// TestErrorMapping verifies A2A error codes are classified via errdefs.
func TestErrorMapping(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(json.RawMessage) (any, *rpcErr) {
		return map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil)}, nil
	}
	f.getFn = func(json.RawMessage) (any, *rpcErr) {
		return nil, &rpcErr{code: -32001, msg: "task not found"}
	}
	eng := newTestEngine(t, f, false, a2a.WithStreamMode(a2a.StreamModeOff), a2a.WithPollInterval(time.Millisecond))

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "test-agent"}, eng, agent.Request{
		Message: message.Message{Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}}},
	}, agent.WithHost(agenttest.NewMockHost()))
	if err != nil {
		t.Fatalf("agent.Execute returned infrastructure error: %v", err)
	}
	if res.Status != agent.StatusFailed || !errdefs.IsNotFound(res.Err) {
		t.Fatalf("status = %q err = %v, want failed with NotFound", res.Status, res.Err)
	}
}

// TestResume verifies a checkpointed run re-attaches to the remote task
// without duplicating the transcript.
func TestResume(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(json.RawMessage) (any, *rpcErr) {
		return map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_WORKING", nil, nil)}, nil
	}
	gets := 0
	f.getFn = func(json.RawMessage) (any, *rpcErr) {
		gets++
		if gets == 1 {
			return taskV1("t1", "c1", "TASK_STATE_WORKING",
				[]any{msgV1("m1", "ROLE_AGENT", textParts("first half"))}, nil), nil
		}
		return taskV1("t1", "c1", "TASK_STATE_COMPLETED",
			[]any{
				msgV1("m1", "ROLE_AGENT", textParts("first half")),
				msgV1("m2", "ROLE_AGENT", textParts("second half")),
			}, nil), nil
	}

	eng := newTestEngine(t, f, false, a2a.WithStreamMode(a2a.StreamModeOff), a2a.WithPollInterval(time.Millisecond))

	// Run 1: task stays working; host captures the checkpoint.
	host1 := agenttest.NewMockHost()
	_ = runTurn(t, eng, "long task", agent.WithHost(host1))
	cps := host1.Checkpoints()
	if len(cps) == 0 {
		t.Fatal("no checkpoints recorded")
	}
	cp := cps[len(cps)-1]

	// Run 2: resume from the checkpoint; the remote is now completed.
	host2 := agenttest.NewMockHost()
	res := runTurn(t, eng, "ignored on resume", agent.WithHost(host2),
		agent.WithResumeFrom(&cp))

	if res.Status != agent.StatusCompleted {
		t.Fatalf("resumed status = %q, want completed", res.Status)
	}
	texts := messageTexts(res.Messages)
	if len(texts) != 2 || texts[0] != "first half" || texts[1] != "second half" {
		t.Errorf("resumed transcript = %v, want [first half second half] (no duplicates)", texts)
	}
}

// TestProtocol03 verifies the a2av0 compat transport speaks the 0.3 wire
// (lowercase enums, kind-discriminated results).
func TestProtocol03(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(params json.RawMessage) (any, *rpcErr) {
		// 0.3 wire: task carries kind + lowercase state.
		return map[string]any{
			"kind":      "task",
			"id":        "t03",
			"contextId": "c03",
			"status":    map[string]any{"state": "completed"},
		}, nil
	}
	eng, err := a2a.New(context.Background(),
		card(f.url(), false, "0.3"),
		a2a.WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatalf("a2a.New: %v", err)
	}
	res := runTurn(t, eng, "legacy hello")
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
}

func textEquals(part message.Part, want string) bool {
	tp, ok := part.(message.TextPart)
	return ok && tp.Text == want
}

func messageTexts(msgs []message.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, p := range m.Content.Parts {
			if tp, ok := p.(message.TextPart); ok {
				out = append(out, tp.Text)
			}
		}
	}
	return out
}
