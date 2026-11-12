package nodes_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// recordLLMCall is the record kind a model call writes; the node package keeps
// it unexported, so the test names it literally — a rename must break this on
// purpose.
const recordLLMCall = "llm.call"

// modelInvoke is the Invoke the engine would pass to a model node, with the two
// policies set to what the case under test asks for.
func modelInvoke(level telemetry.TraceLevel, bodies bool) graph.Invoke {
	state := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
		"text": graph.Text(""),
	}
	invoke := invokeFor(state, `{"messages":"messages","output":"text"}`)
	invoke.Trace = level
	invoke.Bodies = bodies
	return invoke
}

// recordAt returns when a record of the given kind happened.
func recordAt(t *testing.T, result graph.Result, kind string) time.Time {
	t.Helper()
	for _, record := range result.Records {
		if record.Kind == kind {
			return record.At
		}
	}
	t.Fatalf("the result holds no %q record", kind)
	return time.Time{}
}

// callRecords returns every llm.call record a run produced.
func callRecords(t *testing.T, result graph.Result) []agent.LLMCallRecord {
	t.Helper()
	var found []agent.LLMCallRecord
	for _, record := range result.Records {
		if record.Kind != recordLLMCall {
			continue
		}
		var call agent.LLMCallRecord
		if err := json.Unmarshal(record.Payload, &call); err != nil {
			t.Fatalf("decode %s: %v", record.Kind, err)
		}
		found = append(found, call)
	}
	return found
}

// Node level is where the per-call evidence appears: the request hash and the
// prompt digests ride the step's checkpoint, so a run can be attributed and a
// prompt's drift is visible.
func TestModelRecordsAtNodeLevel(t *testing.T) {
	provider := &stubProvider{response: textResponse("ok")}
	kind := nodes.Model(testRuntime(t, provider))

	stream, err := kind.Invoke(context.Background(),
		modelInvoke(telemetry.TraceNode, false))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, stream)
	if len(provider.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.requests))
	}
	calls := callRecords(t, result)
	if len(calls) != 1 {
		t.Fatalf("%s records = %d, want 1", recordLLMCall, len(calls))
	}
	if calls[0].RequestHash == "" {
		t.Error("the record has no request hash")
	}
	if len(calls[0].Messages) == 0 {
		t.Error("the record has no prompt digests")
	}
	if len(calls[0].Response) != 0 || calls[0].ResponseRef != "" {
		t.Error("bodies is off, but the response was stored")
	}
	// The record is dated where the call began, not where the step was saved:
	// LatencyMs is what closes the interval.
	at := recordAt(t, result, recordLLMCall)
	if at.IsZero() {
		t.Fatal("the model call record has no time")
	}
	if at.After(time.Now()) {
		t.Errorf("the call is dated %v, which is in the future", at)
	}
}

// Below node level a run keeps no per-call evidence: the tier is a resolution,
// and a step-level trace is a run nobody asked to inspect call by call.
func TestModelKeepsNoEvidenceBelowNodeLevel(t *testing.T) {
	for _, level := range []telemetry.TraceLevel{telemetry.TraceOff, telemetry.TraceRun, telemetry.TraceStep} {
		kind := nodes.Model(testRuntime(t, &stubProvider{response: textResponse("ok")}))
		stream, err := kind.Invoke(context.Background(), modelInvoke(level, true))
		if err != nil {
			t.Fatalf("Invoke at %s: %v", level, err)
		}
		result, _ := drainNode(t, stream)
		if calls := callRecords(t, result); len(calls) != 0 {
			t.Errorf("a run at %s recorded %d model calls", level, len(calls))
		}
	}
}

// Bodies is the retention switch: the same record, with the answer's bytes, which
// is what a replay answers from.
func TestModelBodiesKeepsTheResponse(t *testing.T) {
	kind := nodes.Model(testRuntime(t, &stubProvider{response: textResponse("kept")}))

	stream, err := kind.Invoke(context.Background(),
		modelInvoke(telemetry.TraceNode, true))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, stream)
	calls := callRecords(t, result)
	if len(calls) != 1 {
		t.Fatalf("%s records = %d, want 1", recordLLMCall, len(calls))
	}
	if len(calls[0].Response) == 0 {
		t.Fatal("bodies is on, but the response was not stored")
	}
	var answered message.Message
	if err := json.Unmarshal(calls[0].Response, &answered); err != nil {
		t.Fatalf("decode the recorded response: %v", err)
	}
	if answered.Content.Text() != "kept" {
		t.Errorf("recorded answer = %q, want %q", answered.Content.Text(), "kept")
	}
}

// A record without bytes cannot be replayed, and the failure must name the switch
// that left them out: the alternative is a JSON decode error that reads like
// corruption.
func TestModelReplayWithoutTheBodySaysSo(t *testing.T) {
	kind := nodes.Model(testRuntime(t, &stubProvider{}))
	state := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
		"text": graph.Text(""),
	}
	invoke := invokeFor(state, `{"messages":"messages","output":"text"}`)
	invoke.Replay = true
	invoke.Lookup = func(string) (agent.LLMCallRecord, bool) {
		// What a node-level record with bodies off looks like: identity, no answer.
		return agent.LLMCallRecord{RequestHash: "sha256:x", StopReason: "stop"}, true
	}
	stream, err := kind.Invoke(context.Background(), invoke)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// The round opens before anything else, including the failure.
	opening, openErr := stream.Next(context.Background())
	if openErr != nil {
		t.Fatalf("the round did not open: %v", openErr)
	}
	if _, ok := opening.(agent.StreamStart); !ok {
		t.Fatalf("first event = %T, want the round's start", opening)
	}
	_, nextErr := stream.Next(context.Background())
	if nextErr == nil {
		t.Fatal("a replay without a body was accepted")
	}
	if !errdefs.IsNotAvailable(nextErr) {
		t.Errorf("error %v is not classified as not-available", nextErr)
	}
	if !strings.Contains(nextErr.Error(), "bodies is off") {
		t.Errorf("error %v does not name the switch that left the body out", nextErr)
	}
}
