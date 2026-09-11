package tui

import (
	"strings"
	"testing"
)

// TestThinkCommandSetsCanonicalLevels covers the /think argument form: a
// canonical level sticks, auto clears it, and an unknown level is refused
// instead of silently leaving a stale selection.
func TestThinkCommandSetsCanonicalLevels(t *testing.T) {
	model := Model{}
	for _, level := range []string{"minimal", "low", "medium", "high", "xhigh"} {
		updated, _, consumed := model.runCommand("/think " + level)
		if !consumed {
			t.Fatalf("/think %s was not consumed", level)
		}
		model = updated
		if model.think != level {
			t.Fatalf("/think %s set %q", level, model.think)
		}
	}

	updated, _, _ := model.runCommand("/think auto")
	model = updated
	if model.think != "" {
		t.Fatalf("/think auto left %q", model.think)
	}

	updated, _, _ = model.runCommand("/think turbo")
	model = updated
	if model.think != "" {
		t.Fatalf("/think turbo left %q", model.think)
	}
	if !lastMessageHas(model, "unknown reasoning level") {
		t.Fatalf("messages = %+v", model.messages)
	}
}

// TestModelCommandRejectsUnknownTarget covers the /model argument form
// without a deployment: auto is always valid, and anything else must be a
// target the assembly exposes.
func TestModelCommandRejectsUnknownTarget(t *testing.T) {
	model := Model{}
	updated, _, consumed := model.runCommand("/model glm/glm-5.3-flash")
	model = updated
	if !consumed {
		t.Fatal("/model was not consumed")
	}
	if model.model != "" {
		t.Fatalf("unknown target must not stick, got %q", model.model)
	}
	if !lastMessageHas(model, "unknown model") {
		t.Fatalf("messages = %+v", model.messages)
	}

	updated, _, _ = model.runCommand("/model auto")
	model = updated
	if model.model != "" {
		t.Fatalf("/model auto left %q", model.model)
	}
}

// TestScenarioDirectivesPassThrough: /start and /next are user text the
// graph reads, so the TUI must not swallow anything it does not own.
func TestScenarioDirectivesPassThrough(t *testing.T) {
	for _, input := range []string{"/start", "/next", "/nope"} {
		updated, _, consumed := Model{}.runCommand(input)
		if consumed {
			t.Fatalf("%s was consumed by the TUI", input)
		}
		if len(updated.messages) != 0 {
			t.Fatalf("%s wrote %+v", input, updated.messages)
		}
	}
}

func TestHelpCommandIsConsumed(t *testing.T) {
	updated, _, consumed := Model{}.runCommand("/help")
	if !consumed || !lastMessageHas(updated, "/model") {
		t.Fatalf("messages = %+v", updated.messages)
	}
}

func TestSelectionRendersAutoWhenUnset(t *testing.T) {
	if got := (Model{}).selection(); got != "model=auto think=auto" {
		t.Fatalf("selection = %q", got)
	}
	got := (Model{model: "glm/glm-5.3-flash", think: "high"}).selection()
	if got != "model=glm/glm-5.3-flash think=high" {
		t.Fatalf("selection = %q", got)
	}
}

func lastMessageHas(model Model, want string) bool {
	if len(model.messages) == 0 {
		return false
	}
	return strings.Contains(model.messages[len(model.messages)-1].Text, want)
}
