package workflow

import "github.com/GizClaw/flowcraft/sdk/model"

// NewTextRequest builds a Request whose Message is a single user text turn.
func NewTextRequest(text string) *Request {
	return &Request{
		Message: model.NewTextMessage(model.RoleUser, text),
		Inputs:  make(map[string]any),
	}
}

// MessageText returns the plain text content of a user message, or "".
func MessageText(m model.Message) string {
	if m.Role != model.RoleUser {
		return ""
	}
	return m.Content()
}
