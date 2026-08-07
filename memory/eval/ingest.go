package main

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	memoryconfig "github.com/GizClaw/flowcraft/memory/config"
	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	"github.com/GizClaw/flowcraft/memory/eval/scenarios"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
)

const maxBatchRunes = 12000

// ingestConversation commits one conversation's turns to memory in session /
// rune-bounded batches, recording the evidence-id -> sequence mapping used
// by k_hit.
func ingestConversation(
	ctx context.Context,
	assembly *memoryconfig.Assembly,
	scenario scenarios.Scenario,
	conversation dataset.Conversation,
	seqByEvidence map[string]map[string]uint64,
	timeout time.Duration,
) error {
	ingestCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		ingestCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	scope := scopeFor(scenario.RuntimeID(), conversation.ID)
	batches := batchTurns(conversation.Turns)
	seq := uint64(0)
	for batchIndex, batch := range batches {
		messages := make([]message.Message, 0, len(batch))
		for _, turn := range batch {
			seq++
			if turn.EvidenceID != "" {
				if seqByEvidence[conversation.ID] == nil {
					seqByEvidence[conversation.ID] = make(map[string]uint64)
				}
				seqByEvidence[conversation.ID][turn.EvidenceID] = seq
			}
			messages = append(messages, turn.Message.Clone())
		}
		if len(messages) == 0 {
			continue
		}
		if err := assembly.System.CommitTurn(ingestCtx, sdkmemory.Turn{
			Scope:          scope,
			ConversationID: conversation.ID,
			IdempotencyKey: fmt.Sprintf("%s/%d", conversation.ID, batchIndex),
			Messages:       messages,
		}); err != nil {
			return fmt.Errorf("commit conversation %s batch %d: %w", conversation.ID, batchIndex, err)
		}
	}
	return nil
}

// batchTurns groups turns by session and by a rune budget so each CommitTurn
// stays inside the extractor's context window.
func batchTurns(turns []dataset.Turn) [][]dataset.Turn {
	var (
		batches        [][]dataset.Turn
		current        []dataset.Turn
		currentSession string
		runes          int
	)
	for _, turn := range turns {
		content := strings.TrimSpace(turn.Message.Content.Text())
		if content == "" {
			continue
		}
		if turn.SessionID != "" && turn.SessionID != currentSession && len(current) > 0 {
			batches = append(batches, current)
			current, currentSession, runes = nil, turn.SessionID, 0
		}
		nextRunes := utf8.RuneCountInString(content)
		if len(current) > 0 && runes+nextRunes > maxBatchRunes {
			batches = append(batches, current)
			current, runes = nil, 0
		}
		turn.Content = message.Content{Parts: append([]message.Part(nil), turn.Message.Content.Parts...)}
		current = append(current, turn)
		runes += nextRunes
		if turn.SessionID != "" {
			currentSession = turn.SessionID
		}
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func scopeFor(runtimeID, conversationID string) sdkmemory.Scope {
	return sdkmemory.Scope{RuntimeID: runtimeID, UserID: conversationID}
}
