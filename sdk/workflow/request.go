package workflow

import "github.com/GizClaw/flowcraft/sdk/model"

// RequestConfig holds optional request-level settings (A2A alignment, etc.).
type RequestConfig struct {
	AcceptedOutputModes []string `json:"accepted_output_modes,omitempty"`
}

// Request is one agent turn: user message plus optional inputs and metadata.
type Request struct {
	TaskID    string
	ContextID string
	RuntimeID string
	RunID     string
	Message   model.Message
	Inputs    map[string]any
	Config    *RequestConfig
	// Extensions carries platform-specific data (e.g. executor options for flowgraph).
	Extensions map[string]any
}
