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

// commitGranularity selects how conversation turns are grouped into
// CommitTurn batches.
type commitGranularity string

const (
	// granularitySession keeps the benchmark default: one batch per session,
	// split by the rune budget.
	granularitySession commitGranularity = "session"
	// granularityExchange mirrors real usage: one batch per user+assistant
	// exchange (consecutive same-role messages stay together).
	granularityExchange commitGranularity = "exchange"
	// granularityTurn commits every message separately.
	granularityTurn commitGranularity = "turn"
)

func parseCommitGranularity(value string) (commitGranularity, error) {
	switch commitGranularity(value) {
	case granularitySession, granularityExchange, granularityTurn:
		return commitGranularity(value), nil
	default:
		return "", fmt.Errorf("--commit-granularity must be session, exchange, or turn")
	}
}

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
	granularity commitGranularity,
) error {
	ingestCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		ingestCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	scope := scopeFor(scenario.RuntimeID(), conversation.ID)
	batches := batchTurns(conversation.Turns, granularity)
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

// batchTurns groups turns into CommitTurn batches. Granularity controls the
// primary boundary (session, user+assistant exchange, or every turn); the
// rune budget and session changes always apply as safety boundaries.
func batchTurns(turns []dataset.Turn, granularity commitGranularity) [][]dataset.Turn {
	var (
		batches        [][]dataset.Turn
		current        []dataset.Turn
		currentSession string
		lastRole       message.Role
		runes          int
	)
	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current, runes = nil, 0
		}
	}
	for _, turn := range turns {
		content := strings.TrimSpace(turn.Message.Content.Text())
		if content == "" {
			continue
		}
		if turn.SessionID != "" && turn.SessionID != currentSession && len(current) > 0 {
			flush()
			currentSession = turn.SessionID
		}
		if granularity == granularityTurn {
			flush()
		}
		if granularity == granularityExchange &&
			turn.Message.Role == message.RoleUser && lastRole == message.RoleAssistant {
			flush()
		}
		nextRunes := utf8.RuneCountInString(content)
		if len(current) > 0 && runes+nextRunes > maxBatchRunes {
			flush()
		}
		turn.Message.Content = message.Content{Parts: append([]message.Part(nil), turn.Message.Content.Parts...)}
		current = append(current, turn)
		runes += nextRunes
		lastRole = turn.Message.Role
		if turn.SessionID != "" {
			currentSession = turn.SessionID
		}
	}
	flush()
	return batches
}

func scopeFor(runtimeID, conversationID string) sdkmemory.Scope {
	return sdkmemory.Scope{RuntimeID: runtimeID, UserID: conversationID}
}
