package inference

// Metadata is attached to every successful operation.
type Metadata struct {
	Model     ModelID    `json:"model"`
	Operation Operation  `json:"operation"`
	Decisions []Decision `json:"decisions,omitempty"`

	// RequestID is the provider-assigned request identifier when the
	// wire response carries one (e.g. DashScope request_id, or an error
	// envelope's request id). Empty when the provider does not expose
	// one. Runtime telemetry mirrors it onto spans as llm.request.id.
	RequestID string `json:"request_id,omitempty"`
	// ResponseID is the provider-assigned identifier of the response
	// object when the wire response carries one (e.g. OpenAI
	// response.id, Anthropic message.id, chat completion id). Empty
	// when unavailable. Runtime telemetry mirrors it onto spans as
	// llm.response.id.
	ResponseID string `json:"response_id,omitempty"`
}

// Clone returns a defensive copy of the metadata: the returned value
// shares no backing array with the receiver.
func (m Metadata) Clone() Metadata {
	m.Decisions = append([]Decision(nil), m.Decisions...)
	return m
}
