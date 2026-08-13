package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
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
}
