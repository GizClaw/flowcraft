package workflow

import "github.com/GizClaw/flowcraft/sdk/model"

// RequestConfig holds optional request-level settings (A2A alignment, etc.).
type RequestConfig struct {
	AcceptedOutputModes []string `json:"accepted_output_modes,omitempty"`
}

// Request is one agent turn: user message plus optional inputs and metadata.
type Request struct {
	TaskID     string         `json:"task_id,omitempty"`
	ContextID  string         `json:"context_id,omitempty"`
	RuntimeID  string         `json:"runtime_id,omitempty"`
	RunID      string         `json:"run_id,omitempty"`
	Message    model.Message  `json:"message"`
	Inputs     map[string]any `json:"inputs,omitempty"`
	Config     *RequestConfig `json:"config,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}
