package bytedance

import (
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

func convertMessages(msgs []model.Message) []*arkmodel.ChatCompletionMessage {
	out := make([]*arkmodel.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		msg := &arkmodel.ChatCompletionMessage{
			Role: convertRole(m.Role),
		}

		if m.Role == model.RoleTool {
			for _, r := range m.ToolResults() {
				s := r.Content
				out = append(out, &arkmodel.ChatCompletionMessage{
					Role:       convertRole(m.Role),
					ToolCallID: r.ToolCallID,
					Content:    &arkmodel.ChatCompletionMessageContent{StringValue: &s},
				})
			}
			continue
		}

		if m.Role == model.RoleAssistant && m.HasToolCalls() {
			var toolCalls []*arkmodel.ToolCall
			for _, tc := range m.ToolCalls() {
				toolCalls = append(toolCalls, &arkmodel.ToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: arkmodel.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
			msg.ToolCalls = toolCalls
			if text := m.Content(); text != "" {
				msg.Content = &arkmodel.ChatCompletionMessageContent{StringValue: &text}
			}
			out = append(out, msg)
			continue
		}

		msg.Content = convertContent(m)
		out = append(out, msg)
	}
	return out
}

func convertRole(r model.Role) string {
	switch r {
	case model.RoleSystem:
		return arkmodel.ChatMessageRoleSystem
	case model.RoleUser:
		return arkmodel.ChatMessageRoleUser
	case model.RoleAssistant:
		return arkmodel.ChatMessageRoleAssistant
	case model.RoleTool:
		return arkmodel.ChatMessageRoleTool
	default:
		return string(r)
	}
}

func convertContent(msg model.Message) *arkmodel.ChatCompletionMessageContent {
	hasImage := false
	var parts []*arkmodel.ChatCompletionMessageContentPart

	for _, p := range msg.Parts {
		switch p.Type {
		case model.PartText:
			parts = append(parts, &arkmodel.ChatCompletionMessageContentPart{
				Type: arkmodel.ChatCompletionMessageContentPartTypeText,
				Text: p.Text,
			})
		case model.PartImage:
			if p.Image == nil {
				continue
			}
			hasImage = true
			parts = append(parts, &arkmodel.ChatCompletionMessageContentPart{
				Type: arkmodel.ChatCompletionMessageContentPartTypeImageURL,
				ImageURL: &arkmodel.ChatMessageImageURL{
					URL: p.Image.URL,
				},
			})
		case model.PartFile:
			if p.File != nil && strings.HasPrefix(p.File.MimeType, "image/") {
				hasImage = true
				parts = append(parts, &arkmodel.ChatCompletionMessageContentPart{
					Type: arkmodel.ChatCompletionMessageContentPartTypeImageURL,
					ImageURL: &arkmodel.ChatMessageImageURL{
						URL: p.File.URI,
					},
				})
			} else if p.File != nil {
				parts = append(parts, &arkmodel.ChatCompletionMessageContentPart{
					Type: arkmodel.ChatCompletionMessageContentPartTypeText,
					Text: p.File.URI,
				})
			}
		case model.PartData:
			if p.Data != nil {
				b, _ := json.Marshal(p.Data.Value)
				parts = append(parts, &arkmodel.ChatCompletionMessageContentPart{
					Type: arkmodel.ChatCompletionMessageContentPartTypeText,
					Text: string(b),
				})
			}
		case model.PartToolCall, model.PartToolResult:
			continue
		}
	}

	if len(parts) == 0 {
		empty := ""
		return &arkmodel.ChatCompletionMessageContent{StringValue: &empty}
	}

	if hasImage {
		return &arkmodel.ChatCompletionMessageContent{ListValue: parts}
	}

	var b strings.Builder
	for _, p := range parts {
		if p != nil && p.Type == arkmodel.ChatCompletionMessageContentPartTypeText {
			b.WriteString(p.Text)
		}
	}
	s := b.String()
	return &arkmodel.ChatCompletionMessageContent{StringValue: &s}
}

func convertResponse(resp arkmodel.ChatCompletionResponse) model.Message {
	if len(resp.Choices) == 0 || resp.Choices[0] == nil {
		return model.Message{Role: model.RoleAssistant}
	}

	msg := resp.Choices[0].Message
	var parts []model.Part

	if len(msg.ToolCalls) > 0 {
		if msg.Content != nil && msg.Content.StringValue != nil && strings.TrimSpace(*msg.Content.StringValue) != "" {
			parts = append(parts, model.Part{Type: model.PartText, Text: *msg.Content.StringValue})
		}
		for _, tc := range msg.ToolCalls {
			if tc == nil {
				continue
			}
			parts = append(parts, model.Part{
				Type: model.PartToolCall,
				ToolCall: &model.ToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		return model.Message{Role: model.RoleAssistant, Parts: parts}
	}

	text := extractMessageText(msg)
	if text == "" {
		return model.Message{Role: model.RoleAssistant}
	}
	return model.NewTextMessage(model.RoleAssistant, text)
}

func extractMessageText(msg arkmodel.ChatCompletionMessage) string {
	if msg.Content == nil {
		return ""
	}
	if msg.Content.StringValue != nil {
		return *msg.Content.StringValue
	}
	if msg.Content.ListValue != nil {
		var b strings.Builder
		for _, p := range msg.Content.ListValue {
			if p != nil && p.Type == arkmodel.ChatCompletionMessageContentPartTypeText {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func extractDeltaText(resp arkmodel.ChatCompletionStreamResponse) string {
	if len(resp.Choices) == 0 || resp.Choices[0] == nil {
		return ""
	}
	return resp.Choices[0].Delta.Content
}

func extractFinishReason(resp arkmodel.ChatCompletionStreamResponse) string {
	if len(resp.Choices) == 0 || resp.Choices[0] == nil {
		return ""
	}
	return string(resp.Choices[0].FinishReason)
}
