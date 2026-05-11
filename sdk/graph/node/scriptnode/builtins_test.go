package scriptnode

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scripts"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
)

// These tests exercise the built-in JS node scripts end-to-end through
// ScriptNode to guarantee the bindings each script depends on actually
// exist. They are intentionally NOT unit tests of the .js bodies in
// isolation: every regression we have seen so far (most recently the
// answer.js -> stream.emit reference to a removed binding) involved a
// script-level API drifting out of sync with the Go-side bindings, and
// only an integration test would have caught it.

const runIDForTests = "test-run"

// streamEvent captures a single Emit call so tests can assert on the
// (type, payload) pair that flowed through ctx.Publisher (and from
// there into host.emit on the script side).
type streamEvent struct {
	Type    string
	Payload any
}

// recordingPublisher implements graph.StreamPublisher and stashes
// every Emit for later inspection. Mirrors the executor-side per-node
// publisher wiring without pulling in the rest of executor.runConfig.
func recordingPublisher() (graph.StreamPublisher, *[]streamEvent) {
	events := &[]streamEvent{}
	pub := graph.StreamPublisherFunc(func(eventType string, payload any) {
		*events = append(*events, streamEvent{Type: eventType, Payload: payload})
	})
	return pub, events
}

// execBuiltin builds a ScriptNode from the named built-in script and
// runs it once with a recording publisher attached so callers can
// assert on host.emit events.
func execBuiltin(t *testing.T, name, nodeID string, config map[string]any, setup func(*graph.Board)) (*[]streamEvent, *graph.Board, error) {
	t.Helper()
	rt := jsrt.New(jsrt.WithPoolSize(1))
	src := scripts.MustGet(name)
	n := New(nodeID, name, src, config, rt)
	pub, events := recordingPublisher()
	board := graph.NewBoard()
	if setup != nil {
		setup(board)
	}
	err := n.ExecuteBoard(graph.ExecutionContext{
		Context:   context.Background(),
		Host:      enginetest.NewMockHost(),
		Publisher: pub,
		RunID:     runIDForTests,
	}, board)
	return events, board, err
}

// ---------- answer.js ----------

// answer.js must surface its rendered output as a stream-delta event
// via host.emit. The previous stream.emit("answer", ...) call pointed
// at a binding that no longer exists; this verifies the replacement
// goes through ctx.Publisher (the per-node channel the executor
// pre-bakes) so the wire format matches what engine.PatternRunStream
// subscribers expect once the executor wraps it.
func TestBuiltin_Answer_EmitsTokenViaHostEmit(t *testing.T) {
	events, board, err := execBuiltin(t, "answer", "ans1", map[string]any{
		"template": "hello {{.name}}",
	}, func(b *graph.Board) {
		b.SetVar("name", "world")
	})
	if err != nil {
		t.Fatalf("answer.js execution failed: %v", err)
	}
	if v, _ := board.GetVar("answer"); v != "hello world" {
		t.Fatalf("answer var = %q, want %q", v, "hello world")
	}
	if len(*events) != 1 {
		t.Fatalf("expected 1 stream event, got %d (%+v)", len(*events), *events)
	}
	ev := (*events)[0]
	if ev.Type != "token" {
		t.Fatalf("event.Type = %q, want %q", ev.Type, "token")
	}
	m, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload shape = %T (%+v)", ev.Payload, ev.Payload)
	}
	if m["content"] != "hello world" {
		t.Fatalf("payload.content = %v, want %q", m["content"], "hello world")
	}
}

// stream:false suppresses the host.emit call but still sets board.answer
// — the option opts out of streaming, not of producing the answer.
func TestBuiltin_Answer_StreamFalseSkipsEmit(t *testing.T) {
	events, board, err := execBuiltin(t, "answer", "ans2", map[string]any{
		"template": "static",
		"stream":   false,
	}, nil)
	if err != nil {
		t.Fatalf("answer.js execution failed: %v", err)
	}
	if v, _ := board.GetVar("answer"); v != "static" {
		t.Fatalf("answer var = %v", v)
	}
	if got := len(*events); got != 0 {
		t.Fatalf("events = %d, want 0 when stream:false", got)
	}
}

// Default keys mode (no template, pull from keys) covers the
// non-template branch of answer.js.
func TestBuiltin_Answer_KeysMode(t *testing.T) {
	events, board, err := execBuiltin(t, "answer", "ans3", map[string]any{
		"keys": []any{"a", "b"},
	}, func(b *graph.Board) {
		b.SetVar("a", "first")
		b.SetVar("b", "second")
	})
	if err != nil {
		t.Fatalf("answer.js execution failed: %v", err)
	}
	if v, _ := board.GetVar("answer"); v != "first\nsecond" {
		t.Fatalf("answer var = %v", v)
	}
	if len(*events) != 1 {
		t.Fatalf("events = %d, want 1", len(*events))
	}
	m, _ := (*events)[0].Payload.(map[string]any)
	if m["content"] != "first\nsecond" {
		t.Fatalf("payload.content = %v", m["content"])
	}
}

// host.emit must be a documented no-op when ctx.Publisher was not
// installed — test harnesses that omit the per-node publisher should
// still be able to run scripts without crashes.
func TestBuiltin_Answer_NilPublisherIsNoOp(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	n := New("ans_no_pub", "answer", scripts.MustGet("answer"), map[string]any{
		"template": "hi",
	}, rt)
	board := graph.NewBoard()
	// ExecutionContext intentionally has no Publisher / Host wiring.
	if err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
		RunID:   runIDForTests,
	}, board); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if v, _ := board.GetVar("answer"); v != "hi" {
		t.Fatalf("answer var = %v", v)
	}
}

// Scripts read the run id via the canonical NewRunInfoBridge "run"
// global; scriptnode installs the bridge with whatever ctx.RunID
// carries. The host bridge intentionally does NOT mirror runID — there
// is one source of truth ("run") for run identity.
func TestScriptNode_RunInfoBridge_RunIDVisibleToScript(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	src := `board.setVar("seen_run_id", run.get_run_id());`
	n := New("rn1", "template", src, nil, rt)
	board := graph.NewBoard()
	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
		RunID:   "rn-42",
	}, board)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if v, _ := board.GetVar("seen_run_id"); v != "rn-42" {
		t.Fatalf("seen_run_id = %v, want %q", v, "rn-42")
	}
}

// Per-step identity lives on the graph-layer "node" global (defined in
// scriptnode/bridge_node.go), not on host or run. Scripts read
// node.id() / node.type() to discover the currently executing node;
// the values reflect what scriptnode.New was constructed with.
func TestScriptNode_NodeBridge_ExposesIDAndType(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	src := `
		board.setVar("seen_node_id", node.id());
		board.setVar("seen_node_type", node.type());
	`
	n := New("approval-step-7", "approval", src, nil, rt)
	board := graph.NewBoard()
	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
	}, board)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if v, _ := board.GetVar("seen_node_id"); v != "approval-step-7" {
		t.Fatalf("seen_node_id = %v, want %q", v, "approval-step-7")
	}
	if v, _ := board.GetVar("seen_node_type"); v != "approval" {
		t.Fatalf("seen_node_type = %v, want %q", v, "approval")
	}
}

// ---------- approval.js ----------

// First entry into approval (no decision, no prior status) must:
//   - set approval_status = "pending"
//   - set approval_request.node_id to the actual node id (NOT undefined,
//     which was the regression with config.__node_id)
//   - surface signal.interrupt("waiting for approval") as
//     errdefs.IsInterrupted so the agent layer pauses the run.
func TestBuiltin_Approval_FirstPassInterruptsWithNodeID(t *testing.T) {
	_, board, err := execBuiltin(t, "approval", "approval-step-7", map[string]any{
		"prompt": "Approve change?",
	}, nil)
	if !errdefs.IsInterrupted(err) {
		t.Fatalf("expected errdefs.IsInterrupted, got: %v", err)
	}
	if v, _ := board.GetVar("approval_status"); v != "pending" {
		t.Fatalf("approval_status = %v", v)
	}
	raw, _ := board.GetVar("approval_request")
	req, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("approval_request shape = %T", raw)
	}
	if req["node_id"] != "approval-step-7" {
		t.Fatalf("approval_request.node_id = %v, want %q (was undefined under the config.__node_id regression)",
			req["node_id"], "approval-step-7")
	}
	if req["prompt"] != "Approve change?" {
		t.Fatalf("approval_request.prompt = %v", req["prompt"])
	}
}

// When the agent resumes the node after the user replied,
// approval_decision is set on the board; approval.js must record it as
// the new status and NOT raise another interrupt.
func TestBuiltin_Approval_DecisionRecorded(t *testing.T) {
	_, board, err := execBuiltin(t, "approval", "ap1", nil, func(b *graph.Board) {
		b.SetVar("approval_decision", "approved")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := board.GetVar("approval_status"); v != "approved" {
		t.Fatalf("approval_status = %v", v)
	}
}

// ---------- iteration.js ----------

// The body script must run once per item with __iteration_item bound,
// and __iteration_result values must accumulate into iteration_results.
func TestBuiltin_Iteration_AccumulatesResults(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	n := New("iter1", "iteration", scripts.MustGet("iteration"), map[string]any{
		"input_key": "items",
		"body_script": `
			var x = board.getVar("__iteration_item");
			board.setVar("__iteration_result", x * 2);
		`,
	}, rt)
	board := graph.NewBoard()
	board.SetVar("items", []any{int64(1), int64(2), int64(3)})
	if err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(), RunID: runIDForTests,
	}, board); err != nil {
		t.Fatalf("exec: %v", err)
	}
	raw, _ := board.GetVar("iteration_results")
	results, ok := raw.([]any)
	if !ok {
		t.Fatalf("iteration_results shape = %T", raw)
	}
	if len(results) != 3 {
		t.Fatalf("results len = %d, want 3 (results=%v)", len(results), results)
	}
}

// A signal.interrupt inside the body script must surface to the agent
// layer as an Interrupted error (mirrors the same contract any direct
// signal.interrupt on a ScriptNode upholds). Before the fix the parent
// iteration script discarded the child's signal and kept iterating;
// the body's stale __iteration_result also leaked into the parent's
// results array. Both are covered here.
func TestBuiltin_Iteration_BodyInterruptSurfaces(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(2))
	n := New("iter2", "iteration", scripts.MustGet("iteration"), map[string]any{
		"input_key": "items",
		"body_script": `
			var idx = board.getVar("__iteration_index");
			if (idx === 1) {
				signal.interrupt("need approval at " + idx);
			}
			board.setVar("__iteration_result", idx);
		`,
	}, rt)
	board := graph.NewBoard()
	board.SetVar("items", []any{int64(10), int64(20), int64(30)})

	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(), RunID: runIDForTests,
	}, board)

	if !errdefs.IsInterrupted(err) {
		t.Fatalf("expected errdefs.IsInterrupted, got: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "need approval at 1") {
		t.Fatalf("error did not carry child message: %v", err)
	}

	raw, _ := board.GetVar("iteration_results")
	results, _ := raw.([]any)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1 (results=%v)", len(results), results)
	}
}
