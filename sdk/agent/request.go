package agent

import "github.com/GizClaw/flowcraft/sdk/model"

// Request is one agent turn submitted to [Run].
//
// Field names and JSON tags mirror the A2A protocol's MessageSendParams
// schema (camelCase: taskId, contextId, …) so requests can be
// serialised across the protocol without translation. The notable
// absence vs sdk/workflow.Request is that Request does not carry a
// RuntimeID (Run is now a stateless function) and does not carry a
// Strategy hint (the engine is supplied directly to Run).
type Request struct {
	// TaskID identifies a long-lived task the request is part of.
	// Empty when the caller is not tracking tasks. Maps to A2A's
	// "taskId".
	TaskID string `json:"taskId,omitempty"`

	// ContextID identifies the conversation / session. Used as the
	// conversation key passed to History.Load / Append. Empty means
	// "no persistent transcript for this turn". Maps to A2A's
	// "contextId".
	ContextID string `json:"contextId,omitempty"`

	// RunID is the host-supplied execution id. When empty Run mints
	// one. The same value is propagated as engine.Run.ID and as the
	// run id attribute on emitted events. Not part of the A2A wire
	// schema — it is an internal correlation key, kept camelCase for
	// stylistic consistency.
	RunID string `json:"runId,omitempty"`

	// Message is the user's turn input (text, parts, attachments).
	Message model.Message `json:"message"`

	// Inputs are arbitrary structured inputs the engine reads off
	// the Board (form fields, parameters, …). They are written under
	// their map keys as Board vars before the engine starts.
	Inputs map[string]any `json:"inputs,omitempty"`

	// Config carries per-request preferences (output modes, …).
	// Maps to A2A's "configuration".
	Config *RequestConfig `json:"configuration,omitempty"`

	// Extensions is host-passed-through metadata. agent does NOT
	// interpret it; engines may read it from Run.Attributes if the
	// host chose to forward it.
	Extensions map[string]any `json:"extensions,omitempty"`
}

// RequestConfig holds per-request preferences. Optional knobs are
// added here rather than on Request to keep Request stable across
// minor versions. JSON keys mirror the A2A MessageSendConfiguration
// schema so requests can flow across the protocol without
// translation.
type RequestConfig struct {
	// AcceptedOutputModes constrains what modalities the caller can
	// receive (e.g. ["text/plain"], ["audio/wav"], …). Engines that
	// can produce multiple modalities consult it to pick one. Maps
	// to A2A's "acceptedOutputModes".
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
}
