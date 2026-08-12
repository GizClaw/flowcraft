package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
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
		normalized, err := NormalizePart(part)
		if err != nil {
			return err
		}
		switch normalized.(type) {
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

func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, part := range m.Content.Parts {
		normalized, err := NormalizePart(part)
		if err != nil {
			continue
		}
		if call, ok := normalized.(ToolCallPart); ok {
			calls = append(calls, call.Call.Clone())
		}
	}
	return calls
}

func (m Message) ToolResults() []ToolResult {
	var results []ToolResult
	for _, part := range m.Content.Parts {
		normalized, err := NormalizePart(part)
		if err != nil {
			continue
		}
		if result, ok := normalized.(ToolResultPart); ok {
			results = append(results, result.Result)
		}
	}
	return results
}

func (m Message) HasToolCalls() bool {
	for _, part := range m.Content.Parts {
		normalized, err := NormalizePart(part)
		if err != nil {
			continue
		}
		if _, ok := normalized.(ToolCallPart); ok {
			return true
		}
	}
	return false
}

// CloneMessages returns a deep copy of msgs. Nil stays nil so callers
// can preserve the usual JSON / len semantics.
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

// LastByRole returns the last message in msgs whose Role matches role.
// The boolean is false when no such message exists. The returned Message
// is the slice element itself (not a deep copy); callers that intend to
// mutate it should call [Message.Clone] first.
//
// Typical use is for graph nodes that need to read a single role-scoped
// turn from a board channel — e.g. "the latest user message on
// MainChannel" — without re-implementing the reverse scan everywhere.
func LastByRole(msgs []Message, role Role) (Message, bool) {
	for _, msg := range slices.Backward(msgs) {
		if msg.Role == role {
			return msg, true
		}
	}
	return Message{}, false
}

// NewTextMessage builds a message carrying a single text part.
func NewTextMessage(role Role, text string) Message {
	return Message{Role: role, Content: Content{Parts: []Part{TextPart{Text: text}}}}
}

// Content is an ordered collection of canonical parts. Intent deliberately
// does not belong here; only InputContent may attach execution intent.
type Content struct {
	Parts []Part `json:"parts"`
}

func (c Content) Clone() Content {
	if c.Parts == nil {
		return Content{}
	}
	cloned := Content{Parts: make([]Part, len(c.Parts))}
	for i, part := range c.Parts {
		if !isNilValue(part) {
			normalized, _ := NormalizePart(part)
			cloned.Parts[i] = normalized.Clone()
		}
	}
	return cloned
}

func (c Content) Validate() error {
	if len(c.Parts) == 0 {
		return fmt.Errorf("content must contain at least one part")
	}
	for i, part := range c.Parts {
		normalized, err := NormalizePart(part)
		if err != nil {
			return fmt.Errorf("content part %d: %w", i, err)
		}
		if err := normalized.Validate(); err != nil {
			return fmt.Errorf("content part %d: %w", i, err)
		}
	}
	return nil
}

func (c Content) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	wire := make([]json.RawMessage, len(c.Parts))
	for i, part := range c.Parts {
		normalized, err := NormalizePart(part)
		if err != nil {
			return nil, fmt.Errorf("content part %d: %w", i, err)
		}
		raw, err := MarshalPart(normalized)
		if err != nil {
			return nil, fmt.Errorf("content part %d: %w", i, err)
		}
		wire[i] = raw
	}
	return json.Marshal(struct {
		Parts []json.RawMessage `json:"parts"`
	}{Parts: wire})
}

func (c *Content) UnmarshalJSON(data []byte) error {
	var wire struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	decoded := Content{Parts: make([]Part, len(wire.Parts))}
	for i, raw := range wire.Parts {
		part, err := UnmarshalPart(raw)
		if err != nil {
			return fmt.Errorf("content part %d: %w", i, err)
		}
		decoded.Parts[i] = part
	}
	if err := decoded.Validate(); err != nil {
		return err
	}
	*c = decoded
	return nil
}

// Text concatenates the message's text parts in order. Non-text parts
// are skipped; it answers "what did the user/assistant say" without
// imposing prose structure on multi-modal content.
func (c Content) Text() string {
	var b strings.Builder
	for _, part := range c.Parts {
		normalized, err := NormalizePart(part)
		if err != nil {
			continue
		}
		if tp, ok := normalized.(TextPart); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

// isNilValue reports whether value is a typed nil (e.g. (*Part)(nil))
// sitting behind an any. reflection.Value.IsNil only works on
// chan/func/interface/map/pointer/slice; this is the safe equivalent
// for an any that may carry one.
func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func decodeStrict(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
