package message

import (
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// PromptSourceMessageMIMEType identifies source-message metadata appended to
// model messages passed to LLM prompts.
const PromptSourceMessageMIMEType = "application/vnd.flowcraft.source-message+json"

// PromptMessageFromSource converts a stored source message into a prompt
// message while preserving the original multi-modal parts. Source-side fields
// are appended as a structured data part so prompt consumers can recover stable
// source IDs without flattening message content.
func PromptMessageFromSource(msg Message) model.Message {
	out := msg.Clone()
	out.Parts = append(out.Parts, model.Part{
		Type: model.PartData,
		Data: &model.DataRef{
			MimeType: PromptSourceMessageMIMEType,
			Value:    sourcePromptMetadata(msg),
		},
	})
	return out
}

func sourcePromptMetadata(msg Message) map[string]any {
	out := map[string]any{
		"seq": msg.Seq,
	}
	if msg.ID != "" {
		out["source_id"] = msg.ID
	}
	if msg.ConversationID != "" {
		out["conversation_id"] = msg.ConversationID
	}
	if !msg.CreatedAt.IsZero() {
		out["created_at"] = msg.CreatedAt.Format(time.RFC3339)
	}
	if metadata := cloneJSONCompatibleMap(msg.Metadata); len(metadata) > 0 {
		out["metadata"] = metadata
	}
	if len(msg.SpanRefs) > 0 {
		out["span_refs"] = sourcePromptSpanRefs(msg.SpanRefs)
	}
	return out
}

func sourcePromptSpanRefs(refs []SpanRef) []any {
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		out = append(out, map[string]any{
			"message_id": ref.MessageID,
			"start":      ref.Start,
			"end":        ref.End,
		})
	}
	return out
}

func cloneJSONCompatibleMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return cloneJSONCompatibleMapBestEffort(in)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return cloneJSONCompatibleMapBestEffort(in)
	}
	return out
}

func cloneJSONCompatibleMapBestEffort(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		cloned, ok := cloneJSONCompatibleValue(value)
		if ok {
			out[key] = cloned
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneJSONCompatibleValue(value any) (any, bool) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, false
	}
	return out, true
}
