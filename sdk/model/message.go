// Package model defines the shared data types used across the SDK:
// multi-modal messages, tool calling protocol, and token usage tracking.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Role identifies who sent a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType identifies the content type within a message Part.
type PartType string

const (
	PartText       PartType = "text"
	PartImage      PartType = "image"
	PartAudio      PartType = "audio"
	PartFile       PartType = "file"
	PartData       PartType = "data"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
)

// MediaRef references an image or audio asset.
type MediaRef struct {
	URL       string `json:"url,omitempty"`
	Base64    string `json:"base64,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

// FileRef references a generic file (URI + MIME), e.g. for A2A-style payloads.
type FileRef struct {
	URI      string `json:"uri"`
	MimeType string `json:"mime_type,omitempty"`
	Name     string `json:"name,omitempty"`
}

// DataRef carries structured JSON-compatible data in a message part.
type DataRef struct {
	MimeType string         `json:"mime_type,omitempty"`
	Value    map[string]any `json:"value"`
}

// ToolCall represents a function call requested by the LLM.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResult carries the result of a tool execution back to the LLM.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error,omitempty"`
}

// Part is a single content unit within a Message.
type Part struct {
	Type       PartType    `json:"type"`
	Text       string      `json:"text,omitempty"`
	Image      *MediaRef   `json:"image,omitempty"`
	Audio      *MediaRef   `json:"audio,omitempty"`
	File       *FileRef    `json:"file,omitempty"`
	Data       *DataRef    `json:"data,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

// Message is a multi-modal, provider-agnostic chat message.
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
}

// Clone returns a deep copy of m. It duplicates the Parts slice and all
// pointer-backed payloads so callers can safely retain or mutate the result.
func (m Message) Clone() Message {
	return Message{
		Role:  m.Role,
		Parts: CloneParts(m.Parts),
	}
}

// CloneMessages returns a deep copy of msgs. Nil stays nil so callers can
// preserve the usual JSON / len semantics.
func CloneMessages(msgs []Message) []Message {
	if msgs == nil {
		return nil
	}
	out := make([]Message, len(msgs))
	for i, msg := range msgs {
		out[i] = msg.Clone()
	}
	return out
}

// Clone returns a deep copy of p.
func (p Part) Clone() Part {
	out := p
	if p.Image != nil {
		image := *p.Image
		out.Image = &image
	}
	if p.Audio != nil {
		audio := *p.Audio
		out.Audio = &audio
	}
	if p.File != nil {
		file := *p.File
		out.File = &file
	}
	if p.Data != nil {
		data := *p.Data
		data.Value = cloneAnyMap(p.Data.Value)
		out.Data = &data
	}
	if p.ToolCall != nil {
		call := *p.ToolCall
		out.ToolCall = &call
	}
	if p.ToolResult != nil {
		result := *p.ToolResult
		out.ToolResult = &result
	}
	return out
}

// CloneParts returns a deep copy of parts.
func CloneParts(parts []Part) []Part {
	if parts == nil {
		return nil
	}
	out := make([]Part, len(parts))
	for i, part := range parts {
		out[i] = part.Clone()
	}
	return out
}

// Content returns the concatenated text of all text parts.
func (m Message) Content() string {
	var s strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartText {
			s.WriteString(p.Text)
		}
	}
	return s.String()
}

// ToolCalls extracts all tool-call parts.
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, p := range m.Parts {
		if p.Type == PartToolCall && p.ToolCall != nil {
			calls = append(calls, *p.ToolCall)
		}
	}
	return calls
}

// ToolResults extracts all tool-result parts.
func (m Message) ToolResults() []ToolResult {
	var results []ToolResult
	for _, p := range m.Parts {
		if p.Type == PartToolResult && p.ToolResult != nil {
			results = append(results, *p.ToolResult)
		}
	}
	return results
}

// HasToolCalls reports whether the message contains any tool calls.
func (m Message) HasToolCalls() bool {
	for _, p := range m.Parts {
		if p.Type == PartToolCall && p.ToolCall != nil {
			return true
		}
	}
	return false
}

// NewTextMessage creates a simple text message.
func NewTextMessage(role Role, text string) Message {
	return Message{
		Role:  role,
		Parts: []Part{{Type: PartText, Text: text}},
	}
}

// NewToolCallMessage creates an assistant message containing tool calls.
func NewToolCallMessage(calls []ToolCall) Message {
	parts := make([]Part, len(calls))
	for i, c := range calls {
		ct := c
		parts[i] = Part{Type: PartToolCall, ToolCall: &ct}
	}
	return Message{Role: RoleAssistant, Parts: parts}
}

// NewToolResultMessage creates a tool-role message with multiple results.
func NewToolResultMessage(results []ToolResult) Message {
	parts := make([]Part, len(results))
	for i, r := range results {
		rt := r
		parts[i] = Part{Type: PartToolResult, ToolResult: &rt}
	}
	return Message{Role: RoleTool, Parts: parts}
}

// NewImageMessage creates a user message with text and an image URL.
func NewImageMessage(role Role, text, imageURL string) Message {
	return Message{
		Role: role,
		Parts: []Part{
			{Type: PartText, Text: text},
			{Type: PartImage, Image: &MediaRef{URL: imageURL}},
		},
	}
}

// Usage represents raw token usage from a single LLM call (Provider layer).
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// TokenUsage tracks cumulative token consumption (includes TotalTokens).
//
// The Model / LatencyMs / CostMicros fields enrich the basic token
// counts so a sandbox host (typically the planned sdk/pod controller)
// can enforce dollar-denominated budgets and per-model rate limits
// without a separate sidecar accumulator. All three are optional: a
// reporter that only knows token counts leaves them zero / empty.
type TokenUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`

	// Model is the resolved LLM model name this usage came from
	// (e.g. "gpt-4o", "claude-3-7-sonnet-20250219"). Empty when the
	// reporter does not know which model produced the call (test
	// engines, aggregate reports). Hosts that bucket usage by model
	// for budgeting / quota enforcement consume this field.
	Model string `json:"model,omitempty"`

	// LatencyMs is the wall-clock latency of the producing call in
	// milliseconds. Zero when not measured. Used by sandbox hosts to
	// surface per-call timing on the same dimension as token counts
	// (avoids a parallel timing channel).
	LatencyMs int64 `json:"latency_ms,omitempty"`

	// CostMicros is the cost of the producing call in micro-units of
	// the host's configured currency (micro-USD = USD * 1_000_000).
	// Integer math is used so cumulative budgets do not drift.
	// Zero when no pricing catalog is configured. Hosts enforcing $
	// budgets accumulate this field.
	CostMicros int64 `json:"cost_micros,omitempty"`
}

// Add returns a new TokenUsage that is the sum of u and other.
//
// Numeric fields (token counts, latency, cost) are summed. Model is
// preserved from u when both are non-empty and disagree (the
// accumulator's identity wins); when one side is empty the other
// fills it in. Adding a per-call delta into a running total therefore
// keeps the running total's model label intact even if the delta
// reports a different model — callers that need per-model breakdowns
// SHOULD bucket by Model before summing.
func (u TokenUsage) Add(other TokenUsage) TokenUsage {
	model := u.Model
	if model == "" {
		model = other.Model
	}
	return TokenUsage{
		InputTokens:  u.InputTokens + other.InputTokens,
		OutputTokens: u.OutputTokens + other.OutputTokens,
		TotalTokens:  u.TotalTokens + other.TotalTokens,
		Model:        model,
		LatencyMs:    u.LatencyMs + other.LatencyMs,
		CostMicros:   u.CostMicros + other.CostMicros,
	}
}

// StreamChunk is an incremental piece of a streaming response.
type StreamChunk struct {
	Role         Role       `json:"role,omitempty"`
	Content      string     `json:"content,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

// ToolDefinition describes a tool for LLM function-calling.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// MarshalToolArgs marshals arguments to a JSON string suitable for ToolCall.Arguments.
func MarshalToolArgs(args any) (string, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshal tool args: %w", err)
	}
	return string(b), nil
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return cloneAnyMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = cloneAny(item)
		}
		return out
	default:
		return v
	}
}
