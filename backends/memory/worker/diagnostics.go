package worker

import (
	"context"
	"errors"
	"fmt"

	docsource "github.com/GizClaw/flowcraft/backends/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// Stats is a snapshot of one processor's cumulative counters.
type Stats struct {
	CommitsProcessed   int64  `json:"commits_processed"`
	FactsPublished     int64  `json:"facts_published"`
	DocumentsProcessed int64  `json:"documents_processed"`
	ChunksPublished    int64  `json:"chunks_published"`
	IndexDeltasApplied int64  `json:"index_deltas_applied"`
	LastError          string `json:"last_error,omitempty"`
}

// ConversationDiagnostics reports one conversation's derivation cursor.
type ConversationDiagnostics struct {
	ConversationID string `json:"conversation_id"`
	Watermark      uint64 `json:"watermark"`
	Behind         bool   `json:"behind"`
}

// ScopeDiagnostics reports one hard scope's derivation state.
type ScopeDiagnostics struct {
	Scope             corememory.Scope          `json:"scope"`
	Conversations     []ConversationDiagnostics `json:"conversations,omitempty"`
	DocumentWatermark uint64                    `json:"document_watermark"`
	DocumentsBehind   bool                      `json:"documents_behind"`
}

// Stats returns the cumulative processor counters.
func (processor *Processor) Stats() Stats {
	if processor == nil {
		return Stats{}
	}
	processor.statsMu.Lock()
	defer processor.statsMu.Unlock()
	return processor.stats
}

// RecordError stores the most recent derivation failure for diagnostics.
func (processor *Processor) RecordError(err error) {
	if processor == nil || err == nil {
		return
	}
	processor.statsMu.Lock()
	processor.stats.LastError = err.Error()
	processor.statsMu.Unlock()
}

func (processor *Processor) bump(update func(*Stats)) {
	processor.statsMu.Lock()
	update(&processor.stats)
	processor.statsMu.Unlock()
}

// Diagnostics reports cursors and pending work for one hard scope without
// mutating derivation state.
func (processor *Processor) Diagnostics(ctx context.Context, scope corememory.Scope) (ScopeDiagnostics, error) {
	if processor == nil {
		return ScopeDiagnostics{}, errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return ScopeDiagnostics{}, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return ScopeDiagnostics{}, err
	}
	result := ScopeDiagnostics{Scope: scope}
	conversations, err := processor.messages.ListConversations(ctx, scope)
	if err != nil {
		return ScopeDiagnostics{}, fmt.Errorf("memory worker: list conversations: %w", err)
	}
	for _, conversationID := range conversations {
		cursor := uint64(0)
		if watermark, found, loadErr := processor.checkpoints.LoadWatermark(
			ctx, scope, streamKindMessages, conversationID, processor.policyDigest,
		); loadErr != nil {
			return ScopeDiagnostics{}, loadErr
		} else if found {
			cursor = watermark.Cursor
		}
		pending, err := processor.messages.ListCommits(ctx, scope, conversationID, msgsource.ListCommitOptions{
			AfterVersion: cursor, Limit: 1,
		})
		if err != nil {
			return ScopeDiagnostics{}, fmt.Errorf("memory worker: pending commits: %w", err)
		}
		result.Conversations = append(result.Conversations, ConversationDiagnostics{
			ConversationID: conversationID, Watermark: cursor, Behind: len(pending) > 0,
		})
	}
	if processor.documents != nil {
		if watermark, found, loadErr := processor.checkpoints.LoadWatermark(
			ctx, scope, streamKindDocuments, documentStreamID, processor.policyDigest,
		); loadErr != nil {
			return ScopeDiagnostics{}, loadErr
		} else if found {
			result.DocumentWatermark = watermark.Cursor
		}
		events, err := processor.documents.ListEvents(ctx, scope, docsource.ListEventOptions{
			AfterOutboxSeq: result.DocumentWatermark, Limit: 1,
		})
		if err != nil {
			return ScopeDiagnostics{}, fmt.Errorf("memory worker: pending document events: %w", err)
		}
		result.DocumentsBehind = len(events) > 0
	}
	return result, nil
}
