package outcome

import "github.com/GizClaw/flowcraft/core/model"

// Metadata is the identity half of one call's outcome: what answered, and can the
// answer be joined to the provider's own logs.
//
// It carries no payload. The request, the assistant turn and the produced parts
// are the response's business; this is what a record, a span or a dashboard needs
// to say "this is the call that came back", and nothing more. The accounting half
// is [Usage], and the two travel together: every result struct in core/inference
// carries both, which is why they share this package.
type Metadata struct {
	// Model is the provider-qualified model that produced the response.
	Model model.ModelID `json:"model"`
	// RequestID is the provider-assigned request identifier when the wire
	// carries one (DashScope request_id, an error envelope's request id).
	// Empty when the provider exposes none.
	RequestID string `json:"request_id,omitempty"`
	// ResponseID is the provider-assigned identifier of the response object
	// (OpenAI response.id, Anthropic message.id, a chat completion id).
	ResponseID string `json:"response_id,omitempty"`
}
