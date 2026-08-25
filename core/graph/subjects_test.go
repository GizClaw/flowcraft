package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

func TestStepActorForIncludesAgentID(t *testing.T) {
	got := stepActorFor("alice", "n1")
	want := "alice.node.n1"
	if got != want {
		t.Fatalf("stepActorFor(alice, n1) = %q, want %q", got, want)
	}

	subject := agent.SubjectStepStart("run-1", got)
	wantSubject := agent.SubjectStepStart("run-1", "alice.node.n1")
	if subject != wantSubject {
		t.Fatalf("subject = %q, want %q", subject, wantSubject)
	}
}

func TestStepActorSubjectCarriesSanitizedAgentSegment(t *testing.T) {
	subject := agent.SubjectStreamDelta("run-1", stepActorFor("acme-agent", "node-7"))
	if !agent.PatternRunStream("run-1").Matches(subject) {
		t.Fatalf("stream subject %q does not match PatternRunStream", subject)
	}
	if agent.SanitiseID("acme-agent") == "" {
		t.Fatal("sanitised agent id unexpectedly empty")
	}
}

func TestStepErrorPayloadCarriesRequestID(t *testing.T) {
	host := &publishHost{}
	g := &Graph{name: "g"}
	info := agent.RunInfo{Identity: agent.Identity{AgentID: "alice", RunID: "run-1"}}
	stepErr := errdefs.WithRequestID(
		errdefs.Validation(errors.New("boom")), "req-ui-1")

	publishStepError(context.Background(), host, g, info, "n1", stepErr)

	if len(host.envs) != 1 {
		t.Fatalf("published = %d envelopes, want 1", len(host.envs))
	}
	var payload StepEventPayload
	if err := host.envs[0].Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Error != "boom" || payload.RequestID != "req-ui-1" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestRunErrorPayloadCarriesRequestID(t *testing.T) {
	host := &publishHost{}
	g := &Graph{name: "g"}
	run := testRun()
	run.ParentRunID = "run-parent"
	run.Attributes = map[string]string{telemetry.AttrToolCallID: "call-5"}
	runErr := errdefs.WithRequestID(
		errdefs.Validation(errors.New("boom")), "req-ui-2")

	if err := publishRunEvent(
		context.Background(), host, g, run,
		agent.SubjectRunEnd(run.RunID), runErr,
	); err != nil {
		t.Fatalf("publishRunEvent: %v", err)
	}

	if len(host.envs) != 1 {
		t.Fatalf("published = %d envelopes, want 1", len(host.envs))
	}
	var payload RunEventPayload
	if err := host.envs[0].Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Error != "boom" || payload.RequestID != "req-ui-2" {
		t.Fatalf("payload = %+v", payload)
	}
	if host.envs[0].ParentRunID() != "run-parent" || host.envs[0].ToolCallID() != "call-5" {
		t.Fatalf("run envelope lineage headers = %+v", host.envs[0].Headers)
	}
}

func TestPublishStreamDelta_StampsLineageHeaders(t *testing.T) {
	host := &publishHost{}
	info := agent.RunInfo{
		Identity: agent.Identity{
			AgentID:     "child",
			RunID:       "run-child",
			ParentRunID: "run-parent",
		},
		Attributes: map[string]string{telemetry.AttrToolCallID: "call-5"},
	}
	if err := publishStreamDelta(context.Background(), host, info, "g", "n1",
		agent.StreamDeltaPayload{
			Type: agent.StreamDeltaPart,
			Part: message.TextPart{Text: "x"},
		}); err != nil {
		t.Fatalf("publishStreamDelta: %v", err)
	}
	env := host.envs[0]
	if env.ParentRunID() != "run-parent" || env.ToolCallID() != "call-5" {
		t.Fatalf("lineage headers = %+v", env.Headers)
	}

	// Top-level runs carry no lineage headers.
	topHost := &publishHost{}
	top := agent.RunInfo{Identity: agent.Identity{AgentID: "root", RunID: "run-root"}}
	if err := publishStreamDelta(context.Background(), topHost, top, "g", "n1",
		agent.StreamDeltaPayload{
			Type: agent.StreamDeltaPart,
			Part: message.TextPart{Text: "x"},
		}); err != nil {
		t.Fatalf("publishStreamDelta: %v", err)
	}
	if topEnv := topHost.envs[0]; topEnv.ParentRunID() != "" || topEnv.ToolCallID() != "" {
		t.Fatalf("top-level run got lineage headers: %+v", topEnv.Headers)
	}
}

func TestPublishStep_StampsLineageHeaders(t *testing.T) {
	host := &publishHost{}
	g := &Graph{name: "g"}
	info := agent.RunInfo{
		Identity: agent.Identity{
			AgentID:     "child",
			RunID:       "run-child",
			ParentRunID: "run-parent",
		},
		Attributes: map[string]string{telemetry.AttrToolCallID: "call-5"},
	}
	publishStepStarted(context.Background(), host, g, info, "n1")
	if len(host.envs) != 1 {
		t.Fatalf("published = %d envelopes, want 1", len(host.envs))
	}
	env := host.envs[0]
	if env.ParentRunID() != "run-parent" || env.ToolCallID() != "call-5" {
		t.Fatalf("step envelope lineage headers = %+v", env.Headers)
	}
}
