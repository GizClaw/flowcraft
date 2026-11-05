package inference

import "github.com/GizClaw/flowcraft/core/inference/model"

// Metadata describes a completed call. It answers "what answered, and can I
// correlate this with the provider's logs" without exposing the request or the
// response payload.
type Metadata struct {
	// Model is the provider-qualified model that produced the response.
	Model model.ModelID `json:"model"`
	// Operation is the call that produced it.
	Operation model.Operation `json:"operation"`
	// RequestID is the provider-assigned request identifier when the wire
	// carries one (DashScope request_id, an error envelope's request id).
	// Empty when the provider exposes none.
	RequestID string `json:"request_id,omitempty"`
	// ResponseID is the provider-assigned identifier of the response object
	// (OpenAI response.id, Anthropic message.id, a chat completion id).
	ResponseID string `json:"response_id,omitempty"`
}
