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

// PatternPromptRequested matches every run's prompt-request event.
func PatternPromptRequested() event.Pattern {
	return event.Pattern(agent.SubjectPrefix + "*.prompt.requested")
}

// PromptStatus is the terminal lifecycle state of one prompt.
type PromptStatus string

const (
	// PromptReplied is published when a Reply satisfied the prompt.
	PromptReplied PromptStatus = "replied"
	// PromptExpired is published when the AskUser context ended before
	// a reply arrived (timeout or host cancellation).
	PromptExpired PromptStatus = "expired"
	// PromptInterrupted is published when a turn interrupt closed the
	// prompt.
	PromptInterrupted PromptStatus = "interrupted"
	// PromptClosed is published when the turn ended while the prompt
	// was still pending, or when the prompt could not be published.
	PromptClosed PromptStatus = "closed"
)

// PromptResolved is published when a prompt stops waiting for user
// input, so consumers (UI views) can close or invalidate the pending
// interaction immediately instead of waiting for the whole turn to end.
// It is a best-effort notification: publish failures do not change the
// Reply / AskUser result.
type PromptResolved struct {
	RunID    string       `json:"run_id"`
	TurnID   string       `json:"turn_id"`
	PromptID string       `json:"prompt_id"`
	Status   PromptStatus `json:"status"`
}

// SubjectPromptResolved returns the run-scoped prompt resolution
// subject, sharing the run namespace with SubjectPromptRequested so a
// "agent.run.<id>.>" subscription observes both.
func SubjectPromptResolved(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.prompt.resolved", agent.SubjectPrefix, agent.SanitiseID(runID)))
}

// PatternPromptResolved matches every run's prompt-resolution event.
func PatternPromptResolved() event.Pattern {
	return event.Pattern(agent.SubjectPrefix + "*.prompt.resolved")
}
