package inference

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// FinishReason is why the model stopped. It is the canonical set; a driver maps
// its provider's vocabulary onto it and reports an unknown value as
// [ProviderResponseUnusable] rather than inventing a reason.
type FinishReason string

const (
	// FinishStop means the model completed its turn.
	FinishStop FinishReason = "stop"
	// FinishLength means the output hit a length limit.
	FinishLength FinishReason = "length"
	// FinishToolCalls means the model stopped in order to call tools.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishContentFilter means the provider withheld output.
	FinishContentFilter FinishReason = "content_filter"
	// FinishError means the provider reported an in-band failure.
	FinishError FinishReason = "error"
)

// Validate reports whether the reason is one the contract names.
func (r FinishReason) Validate() error {
	switch r {
	case FinishStop, FinishLength, FinishToolCalls, FinishContentFilter, FinishError:
		return nil
	default:
		return fmt.Errorf("unknown finish reason %q", r)
	}
}

// Warning reports a requested field the driver could not honor in full. It is
// how a silently dropped option becomes visible: "temperature ignored: this
// model has no sampling controls" instead of a request that looks honored.
//
// A warning is informational. It never means the response is unusable; a field
// that could not be honored at all is an error, not a warning.
type Warning struct {
	// Field is the canonical knob that was adjusted. It may be an extension
	// field identity when the option was provider-specific.
	Field Field `json:"field,omitempty"`
	// Code is a stable, driver-defined machine label for the adjustment
	// ("implicit_off", "quantized", "not_supported").
	Code string `json:"code"`
	// Message is the human explanation.
	Message string `json:"message"`
}

// GenerateResponse is one completed generation.
type GenerateResponse struct {
	// Message is the assistant turn. Its role is always
	// [message.RoleAssistant].
	Message message.Message `json:"message"`
	// FinishReason is why generation stopped.
	FinishReason FinishReason `json:"finish_reason"`
	// Usage is the token and cost accounting the provider reported.
	Usage usage.Usage `json:"usage,omitzero"`
	// Metadata identifies what answered.
	Metadata Metadata `json:"metadata,omitzero"`
	// Warnings lists fields the driver could not honor in full.
	Warnings []Warning `json:"warnings,omitempty"`
}

// Validate checks the response in isolation.
func (r GenerateResponse) Validate() error {
	if r.Message.Role != message.RoleAssistant {
		return fmt.Errorf("response role must be %q", message.RoleAssistant)
	}
	if err := r.Message.Validate(); err != nil {
		return fmt.Errorf("response message: %w", err)
	}
	if err := r.FinishReason.Validate(); err != nil {
		return err
	}
	for i, warning := range r.Warnings {
		if warning.Message == "" {
			return fmt.Errorf("warning %d has no message", i)
		}
		if warning.Field != "" && !warning.Field.Valid() {
			return fmt.Errorf("warning %d names unknown field %q", i, warning.Field)
		}
	}
	return nil
}

// ValidateFor checks the response against the request that produced it: a model
// must not call a tool the request never defined. The first such call is
// reported as [UndefinedTool] and attached to the error so a caller can rebuild
// it.
func (r GenerateResponse) ValidateFor(request GenerateRequest) error {
	if err := r.Validate(); err != nil {
		return inferenceErr(
			ProviderResponseUnusable, OperationGenerate, "", err,
		)
	}
	for _, call := range r.Message.ToolCalls() {
		if !request.Tools.Has(call.Name) {
			return newUndefinedToolError(OperationGenerate, call)
		}
	}
	return nil
}
