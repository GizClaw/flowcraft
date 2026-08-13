package session

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
)

func TestSubjectPromptRequestedUsesSanitizedRunNamespace(t *testing.T) {
	got := SubjectPromptRequested("run.with*>wildcards")
	want := agent.SubjectPrefix + "run_with__wildcards.prompt.requested"
	if string(got) != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("subject validation error = %v", err)
	}
}

func TestPromptResolvedSubjectUsesSanitizedRunNamespace(t *testing.T) {
	got := SubjectPromptResolved("run.with*>wildcards")
	want := agent.SubjectPrefix + "run_with__wildcards.prompt.resolved"
	if string(got) != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("subject validation error = %v", err)
	}
}

func TestPromptPatternsMatchOnlyTheirOwnSubject(t *testing.T) {
	requested := PatternPromptRequested()
	resolved := PatternPromptResolved()
	for _, pattern := range []event.Pattern{requested, resolved} {
		if err := pattern.Validate(); err != nil {
			t.Fatalf("pattern %q validation error = %v", pattern, err)
		}
	}
	if !requested.Matches(SubjectPromptRequested("run-1")) {
		t.Fatal("requested pattern does not match a prompt-requested subject")
	}
	if !resolved.Matches(SubjectPromptResolved("run-1")) {
		t.Fatal("resolved pattern does not match a prompt-resolved subject")
	}
	if requested.Matches(SubjectPromptResolved("run-1")) {
		t.Fatal("requested pattern matches a prompt-resolved subject")
	}
	if resolved.Matches(SubjectPromptRequested("run-1")) {
		t.Fatal("resolved pattern matches a prompt-requested subject")
	}
}
