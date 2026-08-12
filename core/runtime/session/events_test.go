package session

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
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
