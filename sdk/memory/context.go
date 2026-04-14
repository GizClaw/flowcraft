package memory

import "context"

type ctxKeyConvID struct{}

// WithConversationID injects the conversation ID into the context.
func WithConversationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyConvID{}, id)
}

// ConversationIDFrom retrieves the conversation ID from the context.
func ConversationIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyConvID{}).(string)
	return v
}
