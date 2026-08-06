package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// ReasoningEffort is the request-side "how hard should the model think"
// knob. It is a wire-level string enum, but it is an inference
// concept (not a message part, not a tool DTO) so it lives here
// rather than in [github.com/GizClaw/flowcraft/sdk/message].
type ReasoningEffort string

const (
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

// FinishReason tells the caller why a generate call stopped. It is
// part of the inference response envelope, not a property of a
// [github.com/GizClaw/flowcraft/sdk/message.Message], so it lives
// here next to the rest of the inference control types.
type FinishReason string

const (
	FinishCompleted       FinishReason = "completed"
	FinishMaxOutput       FinishReason = "max_output"
	FinishToolCalls       FinishReason = "tool_calls"
	FinishContentFilter   FinishReason = "content_filter"
	FinishRefusal         FinishReason = "refusal"
	FinishPause           FinishReason = "pause"
	FinishInvalidToolCall FinishReason = "invalid_tool_call"
	FinishContextLimit    FinishReason = "context_limit"
	FinishOther           FinishReason = "other"
)

func (r FinishReason) Validate() error {
	switch r {
	case FinishCompleted, FinishMaxOutput, FinishToolCalls, FinishContentFilter,
		FinishRefusal, FinishPause, FinishInvalidToolCall, FinishContextLimit,
		FinishOther:
		return nil
	default:
		return fmt.Errorf("unknown generate finish reason %q", r)
	}
}

// ToolChoiceKind enumerates the request-side strategies for picking
// a tool from the declared [github.com/GizClaw/flowcraft/sdk/message.Definition]s.
type ToolChoiceKind string

const (
	ToolChoiceAuto     ToolChoiceKind = "auto"
	ToolChoiceNone     ToolChoiceKind = "none"
	ToolChoiceRequired ToolChoiceKind = "required"
	ToolChoiceNamed    ToolChoiceKind = "named"
)

// ToolChoice is the inference-side "how should the model pick a
// tool" instruction. The set of available tools still rides in
// TextIntent.Tools as a slice of [github.com/GizClaw/flowcraft/sdk/message.Definition];
// ToolChoice only decides the selection rule.
type ToolChoice struct {
	Kind ToolChoiceKind `json:"kind"`
	Name string         `json:"name,omitempty"`
}

func (c ToolChoice) Validate() error {
	switch c.Kind {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		if c.Name != "" {
			return fmt.Errorf("tool choice %q cannot name a tool", c.Kind)
		}
	case ToolChoiceNamed:
		if c.Name == "" {
			return fmt.Errorf("named tool choice requires a name")
		}
	default:
		return fmt.Errorf("unknown tool choice %q", c.Kind)
	}
	return nil
}

// ResponseFormatKind enumerates the response-shape strategies a
// caller can request.
type ResponseFormatKind string

const (
	ResponseText       ResponseFormatKind = "text"
	ResponseJSONObject ResponseFormatKind = "json_object"
	ResponseJSONSchema ResponseFormatKind = "json_schema"
)

// ResponseFormat is the request-side "shape the response should
// take" knob. Providers that honor it constrain their output to
// the declared shape; providers that don't reject the request.
type ResponseFormat struct {
	Kind   ResponseFormatKind `json:"kind"`
	Name   string             `json:"name,omitempty"`
	Schema json.RawMessage    `json:"schema,omitempty"`
}

func (f ResponseFormat) Validate() error {
	switch f.Kind {
	case ResponseText, ResponseJSONObject:
		if len(f.Schema) != 0 || f.Name != "" {
			return fmt.Errorf("response format %q cannot carry a schema", f.Kind)
		}
	case ResponseJSONSchema:
		schema := bytes.TrimSpace(f.Schema)
		if f.Name == "" || len(schema) == 0 || schema[0] != '{' || !json.Valid(schema) {
			return fmt.Errorf("JSON schema response requires a name and valid schema")
		}
		compiler := newInMemoryJSONSchemaCompiler()
		const resource = "inference://generate-request-schema.json"
		if err := compiler.AddResource(resource, bytes.NewReader(schema)); err != nil {
			return fmt.Errorf("load response JSON schema: %w", err)
		}
		if _, err := compiler.Compile(resource); err != nil {
			return fmt.Errorf("compile response JSON schema: %w", err)
		}
	default:
		return fmt.Errorf("unknown response format %q", f.Kind)
	}
	return nil
}

// newInMemoryJSONSchemaCompiler builds a [jsonschema.Compiler] that
// refuses to load anything over the network. It exists to validate
// user-supplied [ResponseFormat] schemas at request time without
// letting a malicious schema drag in remote resources.
func newInMemoryJSONSchemaCompiler() *jsonschema.Compiler {
	compiler := jsonschema.NewCompiler()
	compiler.LoadURL = func(resource string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("external JSON schema resource %q is not allowed", resource)
	}
	return compiler
}
