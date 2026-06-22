package recent

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	// DefaultWindowID is the descriptor ID used by NewWindow unless overridden.
	DefaultWindowID views.ID = "recent-window"

	// DefaultWindowVersion is the descriptor version used by NewWindow unless overridden.
	DefaultWindowVersion = "v1"
)

const windowErrPrefix = "memory/views/recent/window"

// WindowBudget limits the size of a Window result.
//
// MaxMessages is currently the only supported budget. Token budget support is
// intentionally left out of this skeleton until a tokenizer contract exists.
// MaxMessages == 0 returns no messages, while MaxMessages < 0 means no limit.
type WindowBudget struct {
	MaxMessages int
}

// WindowRequest describes a recent message window load. A nil Budget uses the
// view's default budget; otherwise MaxMessages is interpreted directly, with 0
// meaning no recent messages.
type WindowRequest struct {
	Scope    views.Scope
	Budget   *WindowBudget
	AfterSeq uint64
}

// WindowResult is a read-time view over canonical messages plus evidence refs.
type WindowResult struct {
	Descriptor views.Descriptor
	Messages   []message.Message
	SourceRefs []views.SourceRef
	Truncated  bool
}

// WindowOption configures a Window.
type WindowOption interface {
	applyWindow(*Window)
}

// SummaryDAGOption configures a SummaryDAG.
type SummaryDAGOption interface {
	applySummaryDAG(*SummaryDAG)
}

type descriptorOption struct {
	id      views.ID
	version string
}

// WithID overrides the descriptor ID for Window or SummaryDAG.
func WithID(id views.ID) descriptorOption {
	return descriptorOption{id: id}
}

// WithVersion overrides the descriptor version for Window or SummaryDAG.
func WithVersion(version string) descriptorOption {
	return descriptorOption{version: version}
}

func (o descriptorOption) applyWindow(v *Window) {
	if o.id != "" {
		v.id = o.id
	}
	if o.version != "" {
		v.version = o.version
	}
}

func (o descriptorOption) applySummaryDAG(d *SummaryDAG) {
	if o.id != "" {
		d.id = o.id
	}
	if o.version != "" {
		d.version = o.version
	}
}

type windowDefaultBudgetOption struct {
	budget WindowBudget
}

// WithDefaultBudget sets the budget used when a Window request does not provide one.
func WithDefaultBudget(budget WindowBudget) WindowOption {
	return windowDefaultBudgetOption{budget: budget}
}

func (o windowDefaultBudgetOption) applyWindow(v *Window) {
	v.defaultBudget = o.budget
}

// Window derives recent context from the canonical MessageLog at read time.
//
// It does not persist derived state; the message store remains the source of
// truth.
type Window struct {
	store         message.Store
	id            views.ID
	version       string
	defaultBudget WindowBudget
}

var _ views.View = (*Window)(nil)

// NewWindow creates a recent-message view over store.
func NewWindow(store message.Store, opts ...WindowOption) *Window {
	v := &Window{
		store:         store,
		id:            DefaultWindowID,
		version:       DefaultWindowVersion,
		defaultBudget: WindowBudget{MaxMessages: -1},
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyWindow(v)
		}
	}
	return v
}

// Descriptor returns the view identity.
func (v *Window) Descriptor() views.Descriptor {
	return views.Descriptor{
		ID:      v.id,
		Kind:    views.KindRecentWindow,
		Version: v.version,
	}
}

// Load returns a message window ordered by ascending Seq.
//
// When AfterSeq is zero, MaxMessages selects the last N messages in the
// conversation. When AfterSeq is non-zero, MaxMessages is passed through to the
// message store as a forward window over messages with Seq > AfterSeq.
func (v *Window) Load(ctx context.Context, req WindowRequest) (WindowResult, error) {
	if err := req.Scope.Validate(); err != nil {
		return WindowResult{}, errdefs.Validationf("%s: invalid scope: %w", windowErrPrefix, err)
	}
	conversationID := req.Scope.ConversationID
	if conversationID == "" {
		return WindowResult{}, errdefs.Validationf("%s: conversation_id is required", windowErrPrefix)
	}
	if v.store == nil {
		return WindowResult{}, errdefs.Validationf("%s: message store is required", windowErrPrefix)
	}

	budget := v.effectiveBudget(req.Budget)
	limit := budget.MaxMessages

	messages, err := v.loadMessages(ctx, conversationID, req.AfterSeq, limit)
	if err != nil {
		return WindowResult{}, err
	}

	return WindowResult{
		Descriptor: v.Descriptor(),
		Messages:   messages,
		SourceRefs: sourceRefsForMessages(messages),
		Truncated:  limit > 0 && len(messages) == limit,
	}, nil
}

func (v *Window) effectiveBudget(req *WindowBudget) WindowBudget {
	if req == nil {
		return v.defaultBudget
	}
	return *req
}

func (v *Window) loadMessages(ctx context.Context, conversationID string, afterSeq uint64, limit int) ([]message.Message, error) {
	if limit == 0 {
		return nil, nil
	}
	if afterSeq > 0 {
		return v.store.List(ctx, conversationID, message.ListOptions{
			AfterSeq: afterSeq,
			Limit:    limit,
		})
	}

	messages, err := v.store.List(ctx, conversationID, message.ListOptions{})
	if err != nil {
		return nil, err
	}
	if limit < 0 || len(messages) <= limit {
		return messages, nil
	}
	return messages[len(messages)-limit:], nil
}

func sourceRefsForMessages(messages []message.Message) []views.SourceRef {
	refs := make([]views.SourceRef, 0, len(messages))
	for _, msg := range messages {
		refs = append(refs, views.SourceRef{
			Kind: views.SourceMessage,
			Message: &views.MessageSourceRef{
				ConversationID: msg.ConversationID,
				MessageID:      msg.ID,
			},
		})
	}
	return refs
}
