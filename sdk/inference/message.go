package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/sdk/tool"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role    Role    `json:"role"`
	Content Content `json:"content"`
}

func (m Message) Clone() Message {
	m.Content = m.Content.Clone()
	return m
}

func (m Message) Validate() error {
	switch m.Role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
	default:
		return fmt.Errorf("unknown message role %q", m.Role)
	}
	if len(m.Content.Parts) == 0 {
		return fmt.Errorf("message content is required")
	}
	if err := m.Content.Validate(); err != nil {
		return err
	}
	hasToolResults := false
	for _, part := range m.Content.Parts {
		switch part.(type) {
		case ToolCallPart:
			if m.Role != RoleAssistant {
				return fmt.Errorf("tool call parts require assistant role")
			}
		case ToolResultPart:
			hasToolResults = true
			if m.Role != RoleTool {
				return fmt.Errorf("tool result parts require tool role")
			}
		case ReasoningPart:
			if m.Role != RoleAssistant {
				return fmt.Errorf("reasoning parts require assistant role")
			}
		}
	}
	if m.Role == RoleTool && !hasToolResults {
		return fmt.Errorf("tool role requires a tool result part")
	}
	return nil
}

func (m Message) ToolCalls() []tool.Call {
	var calls []tool.Call
	for _, part := range m.Content.Parts {
		if call, ok := part.(ToolCallPart); ok {
			calls = append(calls, call.Call.Clone())
		}
	}
	return calls
}

func (m Message) ToolResults() []tool.Result {
	var results []tool.Result
	for _, part := range m.Content.Parts {
		if result, ok := part.(ToolResultPart); ok {
			results = append(results, result.Result)
		}
	}
	return results
}

func (m Message) HasToolCalls() bool {
	for _, part := range m.Content.Parts {
		if _, ok := part.(ToolCallPart); ok {
			return true
		}
	}
	return false
}

type ToolChoiceKind string

const (
	ToolChoiceAuto     ToolChoiceKind = "auto"
	ToolChoiceNone     ToolChoiceKind = "none"
	ToolChoiceRequired ToolChoiceKind = "required"
	ToolChoiceNamed    ToolChoiceKind = "named"
)

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

type ResponseFormatKind string

const (
	ResponseText       ResponseFormatKind = "text"
	ResponseJSONObject ResponseFormatKind = "json_object"
	ResponseJSONSchema ResponseFormatKind = "json_schema"
)

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

func newInMemoryJSONSchemaCompiler() *jsonschema.Compiler {
	compiler := jsonschema.NewCompiler()
	compiler.LoadURL = func(resource string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("external JSON schema resource %q is not allowed", resource)
	}
	return compiler
}

type ReasoningEffort string

const (
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

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
