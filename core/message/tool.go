package message

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ToolDefinition describes a callable tool. InputSchema must be a JSON Schema
// object; it is kept as raw JSON so inference requests never expose an
// untyped provider-parameter map.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (d ToolDefinition) Clone() ToolDefinition {
	d.InputSchema = json.RawMessage(bytes.Clone(d.InputSchema))
	return d
}

func (d ToolDefinition) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("tool definition name is required")
	}
	if !isJSONObject(d.InputSchema) {
		return fmt.Errorf("tool %q input schema must be a JSON object", d.Name)
	}
	return nil
}

// ToolCall is a provider-requested tool invocation. Arguments is always a JSON
// object and remains opaque to the inference package.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (c ToolCall) Clone() ToolCall {
	c.Arguments = json.RawMessage(bytes.Clone(c.Arguments))
	return c
}

// NewToolCall encodes args as the argument object for a tool invocation.
func NewToolCall(id, name string, args any) (ToolCall, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return ToolCall{}, fmt.Errorf("marshal tool arguments: %w", err)
	}
	call := ToolCall{ID: id, Name: name, Arguments: raw}
	if err := call.Validate(); err != nil {
		return ToolCall{}, err
	}
	return call, nil
}

func (c ToolCall) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("tool call id is required")
	}
	if c.Name == "" {
		return fmt.Errorf("tool call name is required")
	}
	if !isJSONObject(c.Arguments) {
		return fmt.Errorf("tool call %q arguments must be a JSON object", c.ID)
	}
	return nil
}

// ToolResult carries one tool execution result back into a chat operation.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

func (r ToolResult) Validate() error {
	if r.CallID == "" {
		return fmt.Errorf("tool result call id is required")
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}
