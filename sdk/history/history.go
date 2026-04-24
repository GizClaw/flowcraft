package history

import (
	"context"
	"errors"

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

// ErrClosed is returned by [Coordinator.Shutdown]'s downstream operations
// (Append, Compact, Archive) once Shutdown has been initiated. Callers
// observing this should treat the History as drained and avoid further
// writes; reading via Load remains valid as long as the underlying Store
// is still usable.
var ErrClosed = errors.New("history: coordinator closed")

// Coordinator is the lifecycle + maintenance interface that the [History]
// returned by [NewCompacted] additionally satisfies. All maintenance
// operations (compact, archive, shutdown) are serialized per conversation
// alongside [History.Append], so callers no longer need to coordinate
// locks themselves.
//
// Production callers typically grab it once at construction:
//
//	hist := history.NewCompacted(store, llm, ws)
//	coord, _ := hist.(history.Coordinator)
//	defer func() { _ = coord.Shutdown(context.Background()) }()
//
// Migration: the previous [Closer] sub-interface is retained for one
// release behind a Deprecated marker; new code should prefer Coordinator
// because it offers context-aware shutdown plus first-class maintenance
// entry points that internally share the per-conversation queue used by
// background ingest/archive.
type Coordinator interface {
	// Compact runs DAG compact for one conversation. Serialized against
	// concurrent Append/Archive on the same conversationID.
	Compact(ctx context.Context, conversationID string) (CompactResult, error)
	// Archive runs message archiving for one conversation. Serialized
	// against concurrent Append/Compact on the same conversationID.
	Archive(ctx context.Context, conversationID string) (ArchiveResult, error)
	// Shutdown stops accepting new work and waits for in-flight per-
	// conversation queues to drain. After Shutdown is observed,
	// Append/Compact/Archive return [ErrClosed]; subsequent Shutdown
	// calls block on the same drain and return its result.
	//
	// Shutdown is the canonical "S3" semantics: it does NOT cancel the
	// background workers when ctx expires. A ctx-deadline return value
	// signals "drain in progress, partial work may still finalize after
	// this returns"; the Coordinator stays in the closing/closed state
	// regardless. To observe the eventual drain after a deadline-bounded
	// Shutdown, call Shutdown again with a longer (or unbounded) ctx —
	// the second call falls onto the same drain and returns nil once
	// all workers have exited.
	Shutdown(ctx context.Context) error
}

// Closer is the legacy lifecycle interface that the [History] returned by
// [NewCompacted] still satisfies for one release.
//
// Deprecated: use [Coordinator] (and its context-aware Shutdown) instead.
// Closer.Close has no way to bound its wait, no way to refuse late writes,
// and no way to surface a drain error to the caller. It will be removed
// in v0.3.0.
type Closer interface {
	Close()
}
