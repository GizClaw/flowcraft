package message

import "context"

// AppendRequest appends messages to a single conversation.
type AppendRequest struct {
	ConversationID string
	Messages       []Message
}

// ListOptions controls ordered message scans within a conversation.
type ListOptions struct {
	AfterSeq uint64
	Limit    int
}

// Store persists canonical conversation messages.
type Store interface {
	Append(ctx context.Context, req AppendRequest) ([]Message, error)
	Get(ctx context.Context, conversationID, messageID string) (Message, bool, error)
	List(ctx context.Context, conversationID string, opts ListOptions) ([]Message, error)
	ListConversations(ctx context.Context) ([]string, error)
	DeleteConversation(ctx context.Context, conversationID string) error
}
