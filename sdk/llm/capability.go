package llm

// Capability identifies a feature that can be disabled for a model.
// Black-list semantics: a model "supports" everything by default; a
// catalog declaration / config override flips a Capability into the
// Disabled set to assert "this model cannot do X".
//
// Behavior of each cap when disabled is documented on
// (*capsLLM).filtered and (*capsLLM).preprocessMessages — keep the
// table in doc/sdk-llm-redesign.md §3.5 in sync when adding caps.
type Capability string

const (
	// Generation parameters — disabled means "drop silently from
	// per-call options" (the field is set to nil before the request
	// hits the provider).
	CapTemperature      Capability = "temperature"
	CapTopP             Capability = "top_p"
	CapTopK             Capability = "top_k"
	CapMaxTokens        Capability = "max_tokens"
	CapStopWords        Capability = "stop_words"
	CapFrequencyPenalty Capability = "frequency_penalty"
	CapPresencePenalty  Capability = "presence_penalty"
	CapThinking         Capability = "thinking"

	// JSON output controls — JSONSchema also has a downgrade to JSONMode
	// when disabled (see with_caps.go).
	CapJSONSchema Capability = "json_schema"
	CapJSONMode   Capability = "json_mode"

	// Protocol features — disabling Tools / ToolChoice raises a
	// telemetry warning (the caller's intent to invoke tools is
	// materially altered). CapStreaming triggers a Generate→one-chunk
	// downgrade. CapSystemPrompt triggers system-message folding.
	CapTools         Capability = "tools"
	CapToolChoice    Capability = "tool_choice"
	CapParallelTools Capability = "parallel_tools"
	CapStreaming     Capability = "streaming"
	CapSystemPrompt  Capability = "system_prompt"

	// Input modality caps — disabling these causes Generate /
	// GenerateStream to return errdefs.Validation if the message list
	// contains a part of the matching type. Silent stripping is NOT
	// done; see RFC §10.2.
	CapVision Capability = "vision"
	CapAudio  Capability = "audio"
	CapFile   Capability = "file"
)

// ModelCaps declares which capabilities a model does not support.
// Zero-value means all capabilities are supported.
// Use Supports(cap) instead of inspecting Disabled directly.
type ModelCaps struct {
	Disabled map[Capability]bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
}

// Supports reports whether the model supports the given capability.
func (c ModelCaps) Supports(cap Capability) bool {
	return !c.Disabled[cap]
}

// IsZero reports whether no capabilities are disabled.
func (c ModelCaps) IsZero() bool {
	return len(c.Disabled) == 0
}

// DisabledCaps creates a ModelCaps with the given capabilities disabled.
func DisabledCaps(caps ...Capability) ModelCaps {
	m := ModelCaps{Disabled: make(map[Capability]bool, len(caps))}
	for _, c := range caps {
		m.Disabled[c] = true
	}
	return m
}
