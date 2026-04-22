package history

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// History is the strategy-layer interface that decides which messages to
// return to the LLM and how new turns are persisted. See the package
// doc comment for the per-method contract.
type History interface {
	// Load returns messages suited for the next LLM call. Implementations
	// MAY compress, summarize, or window the underlying transcript.
	//
	// budget is a hint; implementations fall back to their configured
	// defaults when the corresponding [Budget] field is zero. A fully
	// zero Budget explicitly means "use defaults" and is the most
	// common value (the common case: "give me whatever you'd send to
	// the model").
	Load(ctx context.Context, conversationID string, budget Budget) ([]model.Message, error)

	// Append durably persists newMessages — and only newMessages — to the
	// conversation. It MUST be safe to call from multiple goroutines for
	// the same conversationID; implementations serialize per-conversation
	// writes internally. After Append returns nil, the messages are
	// guaranteed visible to subsequent Load calls.
	Append(ctx context.Context, conversationID string, newMessages []model.Message) error

	// Clear removes the conversation and any derived state (summaries,
	// archives) owned by this History implementation.
	Clear(ctx context.Context, conversationID string) error
}

// Budget caps how much transcript [History.Load] returns. Zero means
// "use the implementation default"; set either field to clamp.
type Budget struct {
	// MaxTokens caps the estimated token count of returned messages.
	// Implementations that do not track tokens treat this as a hint.
	MaxTokens int
	// MaxMessages caps the raw message count.
	MaxMessages int
}

// IsZero reports whether b carries no explicit limits.
func (b Budget) IsZero() bool { return b.MaxTokens == 0 && b.MaxMessages == 0 }
