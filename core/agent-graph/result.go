package graph

import (
	"context"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Result is the final assistant message a run produced, plus the state and the
// references a caller needs without reading the whole checkpoint.
func (r *Run) Result(context.Context) (agent.Result, error) {
	result := agent.Result{
		TaskID: r.taskID,
		State:  r.state,
	}
	if msg, ok := r.lastAssistant(); ok {
		result.Content = msg.Content.Clone()
	}
	if r.state == agent.StateInputRequired && len(r.question.Parts) > 0 {
		question := r.question.Clone()
		result.Question = &question
	}
	result.Artifacts = r.artifacts()
	return result, nil
}

// lastAssistant finds the final assistant message in the conversation state.
// The graph may keep messages under a key other than input; the input key wins,
// then the remaining message keys in name order.
func (r *Run) lastAssistant() (message.Message, bool) {
	keys := make([]string, 0, len(r.values.values))
	if r.g.input != "" {
		keys = append(keys, r.g.input)
	}
	rest := make([]string, 0, len(r.values.values))
	for key := range r.values.values {
		if key != r.g.input {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	var fallback *message.Message
	for _, key := range keys {
		value := r.values.values[key]
		if value.Kind() != KindMessages {
			continue
		}
		history, err := value.Messages()
		if err != nil {
			continue
		}
		items := history.Slice()
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].Role != message.RoleAssistant {
				continue
			}
			candidate := items[i]
			if candidate.Content.Text() != "" {
				return candidate, true
			}
			if fallback == nil {
				fallback = &candidate
			}
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return message.Message{}, false
}

// artifacts reports media state values that already point at a stored blob.
// Inline media is not uploaded here: Result is a read.
func (r *Run) artifacts() []agent.Artifact {
	keys := make([]string, 0, len(r.values.values))
	for key := range r.values.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []agent.Artifact
	for _, key := range keys {
		value := r.values.values[key]
		switch value.Kind() {
		case KindImage, KindAudio, KindVideo, KindFile:
		default:
			continue
		}
		media, err := value.Media()
		if err != nil || media.Blob == "" {
			continue
		}
		out = append(out, agent.Artifact{
			ID:        key,
			Name:      key,
			MediaType: media.MediaType,
			Blob:      media.Blob,
		})
	}
	return out
}
