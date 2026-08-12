package graph

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
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
