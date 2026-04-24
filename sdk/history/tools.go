package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// ToolDeps bundles everything the history_expand / history_compact
// tools need at registration time. Pass it to [RegisterTools].
//
// Coordinator is optional but strongly recommended: when set,
// history_compact routes Compact / Archive through the per-conversation
// worker queue, matching the serialization guarantees that
// [Coordinator] offers to first-class callers. When Coordinator is nil
// the tool falls back to constructing an ad-hoc [SummaryDAG] and
// calling [archiveImpl] directly, which can race a concurrent
// [History.Append] on the same conversation. New code that already has
// a [History] from [NewCompacted] should always wire it via:
//
//	coord, _ := hist.(history.Coordinator)
//	history.RegisterTools(registry, history.ToolDeps{
//	    Coordinator:  coord,
//	    SummaryStore: summaryStore,
//	    MessageStore: msgStore,
//	    Workspace:    ws,
//	    Prefix:       prefix,
//	    Config:       cfg,
//	})
type ToolDeps struct {
	// Coordinator, when non-nil, makes history_compact go through the
	// per-conversation queue. Leaving it nil preserves the v0.2 "direct
	// store mutation" behaviour and keeps the struct usable from code
	// that only has raw stores.
	Coordinator Coordinator

	SummaryStore SummaryStore
	MessageStore Store
	Workspace    workspace.Workspace
	Prefix       string
	Config       DAGConfig
}

// RegisterTools registers history_expand and history_compact against the
// supplied [tool.Registry]. The summary index is auto-injected into the
// LLM system prompt via the workflow.VarSummaryIndex board variable, so
// a separate history_search tool is not needed.
//
// See [ToolDeps] for the recommended way to construct deps from an
// existing [History] / [Coordinator] pair.
func RegisterTools(registry *tool.Registry, deps ToolDeps) {
	registry.Register(newHistoryExpandTool(deps))
	registry.RegisterWithScope(newHistoryCompactTool(deps), tool.ScopePlatform)
}

// --- history_expand ---

type historyExpandTool struct {
	deps ToolDeps
}

func newHistoryExpandTool(deps ToolDeps) tool.Tool {
	return &historyExpandTool{deps: deps}
}

func (t *historyExpandTool) Definition() model.ToolDefinition {
	return tool.DefineSchema("history_expand",
		"Expand a compressed summary to see the original messages or finer-grained summaries it was derived from.",
		tool.Property("summary_id", "string", "The ID of the summary to expand"),
		tool.PropertyWithDefault("max_messages", "integer", "Maximum original messages to return", 20),
	).Required("summary_id").Build()
}

func (t *historyExpandTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args struct {
		SummaryID   string `json:"summary_id"`
		MaxMessages int    `json:"max_messages"`
	}
	args.MaxMessages = 20
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("history_expand: parse args: %w", err)
	}

	convID := ConversationIDFrom(ctx)
	if convID == "" {
		return "", fmt.Errorf("history_expand: no conversation ID in context")
	}

	if t.deps.SummaryStore == nil {
		return "", fmt.Errorf("history_expand: summary store not available")
	}

	node, err := t.deps.SummaryStore.GetByConvID(ctx, convID, args.SummaryID)
	if err != nil {
		return "", fmt.Errorf("history_expand: %w", err)
	}

	if node.Depth > 0 {
		var children []*SummaryNode
		for _, sid := range node.SourceIDs {
			child, err := t.deps.SummaryStore.GetByConvID(ctx, convID, sid)
			if err != nil {
				continue
			}
			children = append(children, child)
		}
		return formatChildSummaries(children), nil
	}

	return t.expandLeaf(ctx, convID, node, args.MaxMessages)
}

func (t *historyExpandTool) expandLeaf(ctx context.Context, convID string, node *SummaryNode, maxMsgs int) (string, error) {
	startSeq := node.EarliestSeq
	endSeq := node.LatestSeq + 1

	if t.deps.Workspace != nil {
		archivePrefix := t.deps.Config.Archive.ArchivePrefix
		if archivePrefix == "" {
			archivePrefix = "archive"
		}
		manifest, err := loadManifestImpl(ctx, t.deps.Workspace, t.deps.Prefix, archivePrefix, convID)
		if err == nil && manifest.HotStartSeq > 0 {
			var allMsgs []model.Message

			if startSeq < manifest.HotStartSeq {
				coldEnd := endSeq
				if coldEnd > manifest.HotStartSeq {
					coldEnd = manifest.HotStartSeq
				}
				coldMsgs, err := loadArchivedMessagesImpl(ctx, t.deps.Workspace, t.deps.Prefix, archivePrefix, convID, startSeq, coldEnd-1)
				if err == nil {
					allMsgs = append(allMsgs, coldMsgs...)
				}
			}

			if endSeq > manifest.HotStartSeq {
				hotStart := startSeq - manifest.HotStartSeq
				if hotStart < 0 {
					hotStart = 0
				}
				hotEnd := endSeq - manifest.HotStartSeq
				if rr, ok := t.deps.MessageStore.(RangeReader); ok {
					hotMsgs, err := rr.GetMessageRange(ctx, convID, hotStart, hotEnd)
					if err == nil {
						allMsgs = append(allMsgs, hotMsgs...)
					}
				}
			}

			if len(allMsgs) > 0 {
				if len(allMsgs) > maxMsgs {
					allMsgs = allMsgs[len(allMsgs)-maxMsgs:]
				}
				return formatMessages(allMsgs), nil
			}
		}
	}

	if rr, ok := t.deps.MessageStore.(RangeReader); ok {
		msgs, err := rr.GetMessageRange(ctx, convID, startSeq, endSeq)
		if err == nil {
			if len(msgs) > maxMsgs {
				msgs = msgs[len(msgs)-maxMsgs:]
			}
			return formatMessages(msgs), nil
		}
	}

	msgs, err := t.deps.MessageStore.GetMessages(ctx, convID)
	if err != nil {
		return "", fmt.Errorf("history_expand: get messages: %w", err)
	}
	if startSeq < len(msgs) {
		end := endSeq
		if end > len(msgs) {
			end = len(msgs)
		}
		msgs = msgs[startSeq:end]
	}
	if len(msgs) > maxMsgs {
		msgs = msgs[len(msgs)-maxMsgs:]
	}
	return formatMessages(msgs), nil
}

func formatMessages(msgs []model.Message) string {
	var b strings.Builder
	for _, msg := range msgs {
		text := msg.Content()
		if text != "" {
			fmt.Fprintf(&b, "%s: %s\n\n", msg.Role, text)
		}
	}
	return b.String()
}

func formatChildSummaries(children []*SummaryNode) string {
	var b strings.Builder
	for _, c := range children {
		fmt.Fprintf(&b, "[d%d seq %d-%d] %s\n", c.Depth, c.EarliestSeq, c.LatestSeq, c.Content)
		if c.ExpandHint != "" {
			b.WriteString(c.ExpandHint + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// --- history_compact ---

type historyCompactTool struct {
	deps ToolDeps
}

func newHistoryCompactTool(deps ToolDeps) tool.Tool {
	return &historyCompactTool{deps: deps}
}

func (t *historyCompactTool) Definition() model.ToolDefinition {
	return tool.DefineSchema("history_compact",
		"Manually trigger compact and archive for a conversation's memory DAG.",
		tool.Property("conversation_id", "string", "The conversation ID to compact/archive"),
		tool.PropertyWithDefault("compact", "boolean", "Run DAG compact", true),
		tool.PropertyWithDefault("archive", "boolean", "Run message archiving", true),
	).Required("conversation_id").Build()
}

func (t *historyCompactTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args struct {
		ConversationID string `json:"conversation_id"`
		Compact        *bool  `json:"compact"`
		Archive        *bool  `json:"archive"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("history_compact: parse args: %w", err)
	}

	doCompact := args.Compact == nil || *args.Compact
	doArchive := args.Archive == nil || *args.Archive

	type resultJSON struct {
		CompactResult *CompactResult `json:"compact_result,omitempty"`
		ArchiveResult *ArchiveResult `json:"archive_result,omitempty"`
	}
	var res resultJSON

	// Preferred path: a Coordinator is wired, every operation observes
	// per-conversation serialization, and we never reach into the raw
	// stores from the tool.
	if t.deps.Coordinator != nil {
		if doCompact {
			cr, err := t.deps.Coordinator.Compact(ctx, args.ConversationID)
			if err != nil {
				return "", fmt.Errorf("history_compact: compact: %w", err)
			}
			res.CompactResult = &cr
		}
		if doArchive {
			ar, err := t.deps.Coordinator.Archive(ctx, args.ConversationID)
			if err != nil {
				return "", fmt.Errorf("history_compact: archive: %w", err)
			}
			res.ArchiveResult = &ar
		}
		data, _ := json.Marshal(res)
		return string(data), nil
	}

	// Fallback path (no Coordinator wired): we go straight to the
	// stores, which means a concurrent Append on the same conversation
	// can race the trim step inside archive. Callers that own a
	// [History] from [NewCompacted] should always populate
	// [ToolDeps.Coordinator] to avoid this branch.
	cfg := t.deps.Config
	if doCompact && t.deps.SummaryStore != nil {
		dag := &SummaryDAG{
			store:  t.deps.SummaryStore,
			config: cfg,
		}
		cr, err := dag.Compact(ctx, args.ConversationID)
		if err != nil {
			return "", fmt.Errorf("history_compact: compact: %w", err)
		}
		res.CompactResult = &cr
	}
	if doArchive && t.deps.Workspace != nil && t.deps.MessageStore != nil {
		ar, err := archiveImpl(ctx, t.deps.Workspace, t.deps.MessageStore, t.deps.Prefix, args.ConversationID, cfg.Archive)
		if err != nil {
			return "", fmt.Errorf("history_compact: archive: %w", err)
		}
		res.ArchiveResult = &ar
	}

	data, _ := json.Marshal(res)
	return string(data), nil
}
