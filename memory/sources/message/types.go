package message

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// Message is a canonical source record for one raw conversation message.
type Message struct {
	ID             string
	ConversationID string
	Seq            uint64

	model.Message

	// Metadata must be JSON-compatible. It is persisted with encoding/json,
	// so numeric and container types decode with encoding/json semantics.
	Metadata  map[string]any
	SpanRefs  []SpanRef
	CreatedAt time.Time
}

// SpanRef identifies a byte span within a stored message.
type SpanRef struct {
	MessageID string
	Start     int
	End       int
}

func cloneMessage(m Message) Message {
	m.Message = m.Clone()
	if m.Metadata != nil {
		m.Metadata = cloneAnyMap(m.Metadata)
	}
	if m.SpanRefs != nil {
		m.SpanRefs = append([]SpanRef(nil), m.SpanRefs...)
	}
	return m
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
