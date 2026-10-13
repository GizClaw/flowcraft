package dashscope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// generateRaw is one unary reply, kept as the platform's envelope so the
// decoder reads the payload and the transport reads the accounting and the
// in-band error fields.
type generateRaw struct {
	body generateEnvelope
}

// generateEnvelope is the reply envelope both text surfaces share. The
// platform reports request-level refusals in band — code and message under an
// HTTP 200 — so the transport inspects them before trusting the payload.
type generateEnvelope struct {
	Output struct {
		Choices    []generateChoice    `json:"choices"`
		SearchInfo *generateSearchInfo `json:"search_info"`
	} `json:"output"`
	Usage     generateUsage `json:"usage"`
	RequestID string        `json:"request_id"`
	Code      string        `json:"code"`
	Message   string        `json:"message"`
}

type generateChoice struct {
	FinishReason string              `json:"finish_reason"`
	Message      generateMessageBody `json:"message"`
}

type generateMessageBody struct {
	Role             string                `json:"role"`
	Content          json.RawMessage       `json:"content"`
	ReasoningContent string                `json:"reasoning_content"`
	ToolCalls        []generateToolCallRaw `json:"tool_calls"`
}

type generateToolCallRaw struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// generateUsage is the platform's token accounting: input, output, and total,
// with the reasoning count as a subset of the output and the cache read as a
// subset of the input.
type generateUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// generateSearchInfo is output.search_info: the sources the platform's hosted
// search retrieved for the answer, plus per-entry search fields this driver
// does not surface.
type generateSearchInfo struct {
	SearchResults []generateSearchResult `json:"search_results"`
}

type generateSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	SiteName    string `json:"site_name"`
	PublishTime string `json:"publish_time"`
}

// canonical maps the platform's accounting onto the canonical usage. The
// platform's output_tokens includes the reasoning count, so the subset
// becomes the ReasoningTokens bucket with the documented inclusion
// accounting; the cache read is clamped to the prompt like every other
// self-contradictory wire counter.
func (u generateUsage) canonical() inference.Usage {
	usage := inference.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
	}
	if read := min(max(u.PromptTokensDetails.CachedTokens, 0), u.InputTokens); read > 0 {
		usage.Input.CacheReadTokens = &read
		uncached := u.InputTokens - read
		usage.Input.UncachedTokens = &uncached
	}
	if reasoning := u.OutputTokensDetails.ReasoningTokens; reasoning > 0 {
		usage.Output.ReasoningTokens = &reasoning
		usage.Output.ReasoningAccounting = inference.ReasoningIncludedInOutput
	}
	return usage
}

// output renders the platform's search record as the provider output, and nil
// when the answer carried no sources. A result without a url is not a
// citation and is skipped rather than failing the response.
func (s *generateSearchInfo) output() *SearchOutput {
	if s == nil {
		return nil
	}
	citations := make([]inference.Citation, 0, len(s.SearchResults))
	for _, result := range s.SearchResults {
		if result.URL == "" {
			continue
		}
		citations = append(citations, inference.Citation{
			URL:         result.URL,
			Title:       result.Title,
			SiteName:    result.SiteName,
			PublishTime: result.PublishTime,
		})
	}
	if len(citations) == 0 {
		return nil
	}
	return &SearchOutput{Citations: citations}
}

// transportGenerate posts one unary text-generation call. A non-200 answer is
// already classified by the request path; the in-band error fields of a 200
// answer are classified here so a refusal never surfaces as an empty success.
func transportGenerate(client *mediaClient) inference.Transport[generateWire, generateRaw] {
	return func(ctx context.Context, wire generateWire) (generateRaw, error) {
		var envelope generateEnvelope
		if err := client.postJSON(ctx, wire.path, false, generateRequest(wire), &envelope); err != nil {
			inference.LogProviderCall(ctx, providerID, "generate", wire.model, err, "", "")
			return generateRaw{}, err
		}
		if envelope.Code != "" {
			failure := errorBody{
				Code:      envelope.Code,
				Message:   envelope.Message,
				RequestID: envelope.RequestID,
			}.failure("generate", http.StatusOK)
			inference.LogProviderCall(
				ctx, providerID, "generate", wire.model, failure, envelope.RequestID, "")
			return generateRaw{}, failure
		}
		if len(envelope.Output.Choices) == 0 {
			failure := fmt.Errorf(
				"dashscope: generate returned no choices")
			inference.LogProviderCall(
				ctx, providerID, "generate", wire.model, failure, envelope.RequestID, "")
			return generateRaw{}, failure
		}
		inference.LogProviderCall(
			ctx, providerID, "generate", wire.model, nil, envelope.RequestID, "")
		return generateRaw{body: envelope}, nil
	}
}

// decodeGenerate lowers one unary reply. The platform may report several
// choices; the canonical response carries one message, so any count other
// than one fails the decode rather than dropping an answer.
func decodeGenerate(
	_ context.Context,
	raw generateRaw,
) (inference.GenerateResponse, error) {
	if len(raw.body.Output.Choices) != 1 {
		return inference.GenerateResponse{}, fmt.Errorf(
			"dashscope: generate returned %d choices; the canonical response carries one",
			len(raw.body.Output.Choices),
		)
	}
	choice := raw.body.Output.Choices[0]
	content, err := decodeGenerateContent(choice.Message.Content)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	parts := make([]message.Part, 0, 1+len(choice.Message.ToolCalls))
	if choice.Message.ReasoningContent != "" {
		parts = append(parts, message.ReasoningPart{
			Text: choice.Message.ReasoningContent,
		})
	}
	if content != "" {
		parts = append(parts, message.TextPart{Text: content})
	}
	for _, call := range choice.Message.ToolCalls {
		parts = append(parts, message.ToolCallPart{Call: message.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: json.RawMessage(call.Function.Arguments),
		}})
	}
	response := inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: generateFinish(
			choice.FinishReason, len(choice.Message.ToolCalls) > 0),
		Usage: raw.body.Usage.canonical(),
		Metadata: inference.Metadata{
			RequestID: raw.body.RequestID,
		},
	}
	if output := raw.body.Output.SearchInfo.output(); output != nil {
		response.ProviderOutputs = append(response.ProviderOutputs, output)
	}
	return response, nil
}

// generateFinish maps the wire finish_reason. The streaming surface reports
// the string "null" until the terminal frame, and a reply that carries tool
// calls without a reason infers the tool-call finish; an explicit value
// always wins, and an unknown one degrades to "other" rather than failing.
func generateFinish(raw string, hasToolCalls bool) inference.FinishReason {
	switch raw {
	case "stop":
		return inference.FinishCompleted
	case "length":
		return inference.FinishMaxOutput
	case "tool_calls":
		return inference.FinishToolCalls
	case "content_filter":
		return inference.FinishContentFilter
	case "", "null":
		if hasToolCalls {
			return inference.FinishToolCalls
		}
		return inference.FinishCompleted
	default:
		return inference.FinishOther
	}
}

// decodeGenerateContent flattens a message content value: the text surface
// carries a string, the multimodal surface an array of typed entries, and the
// canonical text output is the concatenated text entries in order. Any other
// entry — an image an interleaved model produced, an audio item — has no
// canonical home on this surface, so it fails the decode rather than being
// dropped silently.
func decodeGenerateContent(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", fmt.Errorf("dashscope: decode content string: %w", err)
		}
		return text, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return "", fmt.Errorf("dashscope: decode content array: %w", err)
	}
	var builder strings.Builder
	for index, item := range items {
		value, ok := item["text"]
		if !ok {
			return "", fmt.Errorf(
				"dashscope: content entry %d is not a text entry", index)
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return "", fmt.Errorf(
				"dashscope: content entry %d text: %w", index, err)
		}
		builder.WriteString(text)
	}
	return builder.String(), nil
}
