package nodes_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/agent/graph/internal/graphtest"
	"github.com/GizClaw/flowcraft/agent/graph/nodes"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/outcome"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/goccy/go-json"
)

// recordLLMCall is the record kind a model call writes; the node package keeps
// it unexported, so the test names it literally — a rename must break this on
// purpose.
const recordLLMCall = "llm.call"

// modelInvoke is the Invoke the engine would pass to a model node, with the two
// policies set to what the case under test asks for.
func modelInvoke(rt *graphtest.Fixture, level telemetry.TraceLevel, bodies bool) graph.Invoke {
	state := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
		"text": graph.Text(""),
	}
	invoke := invokeFor(rt, state, `{"messages":"messages","output":"text"}`)
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
	rt := testRuntime(t, provider)
	kind := nodes.Model()

	stream, err := kind.Invoke(context.Background(),
		modelInvoke(rt, telemetry.TraceNode, false))
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
	for _, level := range []telemetry.TraceLevel{telemetry.TraceNone, telemetry.TraceRun, telemetry.TraceRun} {
		rt := testRuntime(t, &stubProvider{response: textResponse("ok")})
		kind := nodes.Model()
		stream, err := kind.Invoke(context.Background(), modelInvoke(rt, level, true))
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
	rt := testRuntime(t, &stubProvider{response: textResponse("kept")})
	kind := nodes.Model()

	stream, err := kind.Invoke(context.Background(),
		modelInvoke(rt, telemetry.TraceNode, true))
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
	rt := testRuntime(t, &stubProvider{})
	kind := nodes.Model()
	state := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
		"text": graph.Text(""),
	}
	invoke := invokeFor(rt, state, `{"messages":"messages","output":"text"}`)
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

// TestAWholeResponseStillReportsItsUsage is the accounting gap the streaming path hid: a provider
// that answers in one piece sends no usage delta, so the totals exist only in the response. A turn
// that never hears them reports zero tokens, zero cost and a billing line that says the call was
// free — for every non-streaming deployment.
func TestAWholeResponseStillReportsItsUsage(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "streaming"
		if !stream {
			name = "unary"
		}
		t.Run(name, func(t *testing.T) {
			response := textResponse("ok")
			response.Usage = outcome.Usage{InputTokens: 9, OutputTokens: 4}
			provider := &stubProvider{response: response}
			rt := testRuntime(t, provider)

			config := `{"messages":"messages","output":"text"}`
			if !stream {
				config = `{"messages":"messages","output":"text","request":{"stream":false}}`
			}
			stream_, err := nodes.Model().Invoke(context.Background(),
				invokeFor(rt, map[string]graph.Value{
					"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
						message.NewTextMessage(message.RoleUser, "hi"),
					})),
					"text": graph.Text(""),
				}, config))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			_, events := drainNode(t, stream_)

			var usage *outcome.Usage
			for _, event := range events {
				if reported, ok := event.(agent.UsageEvent); ok {
					usage = &reported.Usage
				}
			}
			if usage == nil {
				t.Fatalf("no usage event among %d events; a caller would bill this call as free", len(events))
			}
			if usage.InputTokens != 9 || usage.OutputTokens != 4 {
				t.Fatalf("usage = %+v, want the response's totals", *usage)
			}
		})
	}
}
