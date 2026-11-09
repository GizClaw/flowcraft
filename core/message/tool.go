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
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Arguments Arguments `json:"arguments"`
}

// Arguments is a tool call's argument object.
//
// The bytes are unexported and every constructor copies them, so an argument
// that is already in a transcript cannot be changed by a caller still holding the
// source. A reader decodes it or asks for a copy.
type Arguments struct {
	raw json.RawMessage
}

// NewArguments wraps an encoded argument object. The bytes are copied.
func NewArguments(raw json.RawMessage) Arguments {
	return Arguments{raw: append(json.RawMessage(nil), raw...)}
}

// MarshalJSON returns the encoded object.
func (a Arguments) MarshalJSON() ([]byte, error) { return a.raw, nil }

// UnmarshalJSON keeps a copy of the encoded object.
func (a *Arguments) UnmarshalJSON(data []byte) error {
	a.raw = append(json.RawMessage(nil), data...)
	return nil
}

// Decode unmarshals the object into v.
func (a Arguments) Decode(v any) error { return json.Unmarshal(a.raw, v) }

// Raw returns a copy of the encoded object.
func (a Arguments) Raw() []byte { return append([]byte(nil), a.raw...) }

// String returns the encoded object as a string.
func (a Arguments) String() string { return string(a.raw) }

// Len reports the encoded size in bytes.
func (a Arguments) Len() int { return len(a.raw) }

// IsZero reports whether the call carried no arguments.
func (a Arguments) IsZero() bool { return len(a.raw) == 0 }

// clone returns an independent copy.
func (a Arguments) clone() Arguments {
	return Arguments{raw: append(json.RawMessage(nil), a.raw...)}
}

func (c ToolCall) Clone() ToolCall {
	c.Arguments = c.Arguments.clone()
	return c
}

// NewToolCall encodes args as the argument object for a tool invocation.
func NewToolCall(id, name string, args any) (ToolCall, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return ToolCall{}, fmt.Errorf("marshal tool arguments: %w", err)
	}
	call := ToolCall{ID: id, Name: name, Arguments: NewArguments(raw)}
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
	if !isJSONObject(c.Arguments.raw) {
		return fmt.Errorf("tool call %q arguments must be a JSON object", c.ID)
	}
	return nil
}

// ToolResult carries one tool execution result back into a chat
// operation. Content is the ordered multimodal payload: a text-only
// tool returns a single text part (see [NewTextContent]), while image,
// audio, file, and structured-data results keep their typed parts
// instead of being flattened into a string at the tool boundary.
type ToolResult struct {
	CallID  string  `json:"call_id"`
	Content Content `json:"content"`
	IsError bool    `json:"is_error,omitempty"`
}

func (r ToolResult) Clone() ToolResult {
	r.Content = r.Content.Clone()
	return r
}

func (r ToolResult) Validate() error {
	if r.CallID == "" {
		return fmt.Errorf("tool result call id is required")
	}
	if err := r.Content.Validate(); err != nil {
		return fmt.Errorf("tool result %q: %w", r.CallID, err)
	}
	return nil
}

// NewToolResult pairs a call id with its result content.
func NewToolResult(callID string, content Content) ToolResult {
	return ToolResult{CallID: callID, Content: content}
}

// NewTextToolResult builds a successful result whose content is a
// single text part — the shape a text-only tool returns.
func NewTextToolResult(callID, text string) ToolResult {
	return ToolResult{CallID: callID, Content: NewTextContent(text)}
}

// NewErrorToolResult builds a failed result whose content is a single
// text part. Tool failures are model-visible messages, so the text is
// the message itself, not a diagnostic dump.
func NewErrorToolResult(callID, text string) ToolResult {
	return ToolResult{CallID: callID, Content: NewTextContent(text), IsError: true}
}

func isJSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}
