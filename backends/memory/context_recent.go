package memory

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textutil"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// contextRecent serves the deterministic recent-message lane of one
// conversation. It is the fallback used when no hybrid provider is built.
func (assembly *Assembly) contextRecent(ctx context.Context, request corememory.ContextRequest) (corememory.ContextResult, error) {
	if assembly == nil || assembly.messages == nil {
		return corememory.ContextResult{}, corememory.NewError(corememory.KindNotConfigured, "context", errors.New("memory assembly is incomplete"))
	}
	if ctx == nil {
		return corememory.ContextResult{}, corememory.NewError(corememory.KindInvalidRequest, "context", errors.New("context is required"))
	}
	if err := request.Validate(); err != nil {
		return corememory.ContextResult{}, err
	}
	if strings.TrimSpace(request.ConversationID) == "" {
		// The recent lane is conversation-scoped; without a conversation
		// there is nothing to serve in this scope.
		return corememory.ContextResult{}, nil
	}
	maxItems := request.RecentLimit
	if maxItems <= 0 {
		maxItems = request.Budget.MaxItems
	}
	if maxItems > retrieval.MaxRecentItems {
		maxItems = retrieval.MaxRecentItems
	}
	if maxItems <= 0 {
		maxItems = assembly.recent.MaxItems
	}
	if maxItems <= 0 {
		maxItems = defaultRecentMaxItems
	}
	maxTokens := request.RecentMaxTokens
	if maxTokens <= 0 {
		maxTokens = request.Budget.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = assembly.recent.MaxTokens
	}
	if maxTokens > retrieval.MaxRecentTokens {
		maxTokens = retrieval.MaxRecentTokens
	}
	if maxTokens <= 0 {
		maxTokens = defaultRecentMaxTokens
	}
	records, err := assembly.messages.Latest(ctx, request.Scope, request.ConversationID, msgsource.LatestOptions{
		Limit: maxItems + 1,
	})
	if err != nil {
		return corememory.ContextResult{}, classify(err, "context", corememory.KindProviderFailure)
	}
	result, err := packRecent(records, maxItems, maxTokens, request.Budget.MaxChars)
	if err != nil {
		return corememory.ContextResult{}, corememory.NewError(corememory.KindInternal, "context", err)
	}
	if err := result.Validate(); err != nil {
		return corememory.ContextResult{}, corememory.NewError(corememory.KindInternal, "context", err)
	}
	return result, nil
}

// packRecent converts canonical records into scored, budgeted context items.
// Records arrive in ascending sequence order; selection walks from the
// newest record backwards so a tight budget keeps the most recent turns, and
// the returned items are restored to ascending order. The newest record
// always survives: when it alone exceeds the budget its content is truncated
// to fit and the result reports Truncated, so recall never silently drops the
// most recent turn and never injects an unbounded item. Recent items carry no
// query relevance (Score 1) and are therefore exempt from
// ContextRequest.MinScore, matching the hybrid path where recent candidates
// bypass the threshold.
func packRecent(
	records []msgsource.Record,
	maxItems, maxTokens, maxChars int,
) (corememory.ContextResult, error) {
	selected := make([]corememory.ContextItem, 0, len(records))
	tokens := 0
	chars := 0
	truncated := false
	for index := len(records) - 1; index >= 0; index-- {
		record := records[index]
		item, err := recentItem(record)
		if err != nil {
			return corememory.ContextResult{}, err
		}
		item.TokenCount = textutil.ContentTokens(record.Message.Content)
		if len(selected) == 0 {
			// The newest turn is bounded, never dropped.
			bounded, cut := retrieval.TruncateItemContent(item, maxTokens, maxChars)
			if cut {
				item = bounded
				truncated = true
			}
		}
		itemTokens := item.TokenCount
		itemChars := utf8.RuneCountInString(item.Content.Text())
		if len(selected) > 0 {
			if maxItems > 0 && len(selected) >= maxItems {
				truncated = true
				break
			}
			if maxTokens > 0 && tokens+itemTokens > maxTokens {
				truncated = true
				break
			}
			if maxChars > 0 && chars+itemChars > maxChars {
				truncated = true
				break
			}
		}
		selected = append(selected, item)
		tokens += itemTokens
		chars += itemChars
	}
	items := make([]corememory.ContextItem, len(selected))
	for index := range selected {
		items[len(selected)-1-index] = selected[index]
	}
	return corememory.ContextResult{Items: items, TokenCount: tokens, Truncated: truncated}, nil
}

func recentItem(record msgsource.Record) (corememory.ContextItem, error) {
	if err := record.Message.Validate(); err != nil {
		return corememory.ContextItem{}, err
	}
	item := corememory.ContextItem{
		ID: record.ID,
		Address: corememory.ContextAddress{
			Kind:           corememory.ContextRawMessage,
			ConversationID: record.ConversationID,
			ItemID:         record.ID,
		},
		Kind:        corememory.ContextRawMessage,
		Content:     record.Message.Content.Clone(),
		Score:       1,
		Sources:     []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: record.ID, Revision: strconv.FormatUint(record.Seq, 10)}},
		Metadata:    record.Metadata.Clone(),
		SourceClass: corememory.ContextSourceRecent,
		MessageRole: record.Message.Role,
		Sequence:    record.Seq,
		Timestamp:   record.CreatedAt,
	}
	if err := item.Validate(); err != nil {
		return corememory.ContextItem{}, err
	}
	return item, nil
}
