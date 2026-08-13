package session

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
)

// PromptRequested is published when a turn begins waiting for user input.
type PromptRequested struct {
	RunID    string           `json:"run_id"`
	TurnID   string           `json:"turn_id"`
	PromptID string           `json:"prompt_id"`
	Prompt   agent.UserPrompt `json:"prompt"`
}

// SubjectPromptRequested returns the run-scoped prompt request subject.
func SubjectPromptRequested(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.prompt.requested", agent.SubjectPrefix, agent.SanitiseID(runID)))
}
