package workflow

import "github.com/GizClaw/flowcraft/sdk/model"

// TaskStatus is the terminal or intermediate status of a run.
type TaskStatus string

const (
	StatusCompleted     TaskStatus = "completed"
	StatusWorking       TaskStatus = "working"
	StatusFailed        TaskStatus = "failed"
	StatusInputRequired TaskStatus = "input_required"
	StatusCanceled      TaskStatus = "canceled"
	StatusInterrupted   TaskStatus = "interrupted"
	StatusAborted       TaskStatus = "aborted"
)

// Artifact is a named bundle of parts produced during a run.
type Artifact struct {
	Name  string       `json:"name"`
	Parts []model.Part `json:"parts,omitempty"`
}

// Result is returned by Runtime.Run after execution and finish logic.
type Result struct {
	TaskID    string
	Status    TaskStatus
	Messages  []model.Message
	Artifacts []Artifact
	Usage     model.TokenUsage
	State     map[string]any
	Err       error
	// LastBoard is the board after Execute (for platform persistence hooks). Not serialized.
	LastBoard *Board `json:"-"`
}

// Text returns the last assistant text message in Messages, or "".
func (r *Result) Text() string {
	if r == nil {
		return ""
	}
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != model.RoleAssistant {
			continue
		}
		t := r.Messages[i].Content()
		if t != "" {
			return t
		}
	}
	return ""
}
