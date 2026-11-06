package openai

import (
	"context"
	"encoding/json"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
)

// The chat wire carries no content-part index, so the driver assigns one by
// kind: reasoning is part 0, the visible answer is part 1, and tool call n is
// part 2+n. The assembler then rebuilds the message in that order, which is the
// order a caller reads it.
const (
	chatReasoningIndex = 0
	chatTextIndex      = 1
	chatToolIndexBase  = 2
)

// chatStream adapts the SDK's server-sent-event stream to the protocol's
// wire-level stream.

// chatOpen starts the streaming call.
func (d *driver) chatOpen(
	ctx context.Context,
	request inference.GenerateRequest,
	params openai.ChatCompletionNewParams,
) (protocol.RawStream[openai.ChatCompletionChunk], error) {
	extra, err := extensionJSONSet(request.Extensions)
	if err != nil {
		return nil, err
	}
	opts, err := bodyOptions(params, extra)
	if err != nil {
		return nil, err
	}
	auth, err := d.authOptions(request.Model.Profile)
	if err != nil {
		return nil, err
	}
	stream := d.client.Chat.Completions.NewStreaming(ctx, params, append(opts, auth...)...)
	return newSDKStream(stream.Next, stream.Current, stream.Err, stream.Close), nil
}

// chatStreamDecoder builds the per-stream decoder. The first chunk carries the
// response identity, which is emitted once as a start event so the assembled
// response and the caller's telemetry can name what answered.
func chatStreamDecoder(_ inference.GenerateRequest) protocol.StreamDecoder[openai.ChatCompletionChunk] {
	started := false
	return func(_ context.Context, chunk openai.ChatCompletionChunk) ([]inference.StreamEvent, error) {
		return chatStreamEvents(chunk, &started)
	}
}

// chatStreamEvents maps one wire chunk onto canonical events. A chunk that
// carries nothing a caller cares about yields no events.
func chatStreamEvents(
	chunk openai.ChatCompletionChunk,
	started *bool,
) ([]inference.StreamEvent, error) {
	var events []inference.StreamEvent
	if !*started && chunk.ID != "" {
		*started = true
		metadata := inference.Metadata{
			Model:      model.ModelID{Provider: ProviderID, Name: chunk.Model},
			ResponseID: chunk.ID,
		}
		events = append(events, inference.StreamEvent{
			Kind:     inference.StreamStart,
			Metadata: &metadata,
		})
	}
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if reasoning := reasoningContent(delta); reasoning != "" {
			events = append(events, inference.StreamEvent{
				Kind:  inference.StreamReasoning,
				Index: chatReasoningIndex,
				Text:  reasoning,
			})
		}
		if delta.Content != "" {
			events = append(events, inference.StreamEvent{
				Kind:  inference.StreamText,
				Index: chatTextIndex,
				Text:  delta.Content,
			})
		}
		for _, call := range delta.ToolCalls {
			events = append(events, inference.StreamEvent{
				Kind:  inference.StreamToolCall,
				Index: chatToolIndexBase + int(call.Index),
				Tool: &inference.ToolCallDelta{
					ID:        call.ID,
					Name:      call.Function.Name,
					Arguments: call.Function.Arguments,
				},
			})
		}
		if choice.FinishReason != "" {
			finish, err := chatFinishReason(choice.FinishReason, 0)
			if err != nil {
				return nil, err
			}
			events = append(events, inference.StreamEvent{
				Kind:   inference.StreamEnd,
				Finish: finish,
			})
		}
	}
	// With stream_options.include_usage the provider appends a final chunk that
	// carries usage and no choices; it arrives after the finish reason.
	if chunk.Usage.TotalTokens > 0 {
		accounting := chatChunkUsage(chunk.Usage)
		events = append(events, inference.StreamEvent{
			Kind:  inference.StreamUsage,
			Usage: &accounting,
		})
	}
	return events, nil
}

// reasoningContent reads the dialect's reasoning channel. OpenAI-compatible
// servers disagree on the field name — DeepSeek sends reasoning_content next to
// the answer — and the SDK does not model it, so it is recovered from the
// chunk's raw JSON rather than being lost.
func reasoningContent(delta openai.ChatCompletionChunkChoiceDelta) string {
	raw := delta.RawJSON()
	if raw == "" {
		return ""
	}
	var decoded struct {
		ReasoningContent string `json:"reasoning_content"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return ""
	}
	return decoded.ReasoningContent
}

func chatChunkUsage(raw openai.CompletionUsage) usage.Usage {
	out := usage.Usage{
		InputTokens:  raw.PromptTokens,
		OutputTokens: raw.CompletionTokens,
		TotalTokens:  raw.TotalTokens,
	}
	if cached := raw.PromptTokensDetails.CachedTokens; cached > 0 {
		out.Input.CacheReadTokens = &cached
	}
	return out
}
