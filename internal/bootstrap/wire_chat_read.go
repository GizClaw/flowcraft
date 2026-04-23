package bootstrap

import (
	"github.com/GizClaw/flowcraft/internal/api"
	chatproj "github.com/GizClaw/flowcraft/internal/projection/chat"
)

// chatReadAdapter adapts the chat projector to the api.ChatReadModel
// interface so the api package does not need to import projection/chat
// directly. After R5 this is the only source of truth for
// /api/conversations/{id}/messages.
type chatReadAdapter struct {
	p *chatproj.ChatProjector
}

func newChatReadAdapter(p *chatproj.ChatProjector) api.ChatReadModel {
	if p == nil {
		return nil
	}
	return &chatReadAdapter{p: p}
}

func (a *chatReadAdapter) GetConversation(id string) *api.ChatConversationView {
	conv := a.p.GetConversation(id)
	if conv == nil {
		return nil
	}
	out := &api.ChatConversationView{
		ID:            conv.ID,
		LastMessageAt: conv.LastMessageAt,
		MessageCount:  conv.MessageCount,
		Messages:      make([]api.ChatMessageView, 0, len(conv.Messages)),
	}
	for _, m := range conv.Messages {
		out.Messages = append(out.Messages, api.ChatMessageView{
			MessageID: m.MessageID,
			Role:      m.Role,
			Content:   m.Content,
			SentAt:    m.SentAt,
		})
	}
	return out
}
