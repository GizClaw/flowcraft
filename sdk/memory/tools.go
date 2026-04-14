package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// ToolDeps holds dependencies for memory tools.
type ToolDeps struct {
	SummaryStore SummaryStore
	MessageStore Store
	Workspace    workspace.Workspace
	Prefix       string
	Config       DAGConfig
}

// RegisterTools registers memory_expand and memory_compact.
// Summary index is now auto-injected into the LLM system prompt via
// workflow.VarSummaryIndex board variable, so memory_search is no longer needed.
func RegisterTools(registry *tool.Registry, deps ToolDeps) {
	registry.Register(newMemoryExpandTool(deps))
	registry.RegisterWithScope(newMemoryCompactTool(deps), tool.ScopePlatform)
}

// --- memory_expand ---

type memoryExpandTool struct {
	deps ToolDeps
}

func newMemoryExpandTool(deps ToolDeps) tool.Tool {
	return &memoryExpandTool{deps: deps}
}

func (t *memoryExpandTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "memory_expand",
		Description: "Expand a compressed summary to see the original messages or finer-grained summaries it was derived from.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary_id": map[string]any{
					"type":        "string",
					"description": "The ID of the summary to expand",
				},
				"max_messages": map[string]any{
					"type":        "integer",
					"description": "Maximum original messages to return",
					"default":     20,
				},
			},
			"required": []string{"summary_id"},
		},
	}
}

func (t *memoryExpandTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args struct {
		SummaryID   string `json:"summary_id"`
		MaxMessages int    `json:"max_messages"`
	}
	args.MaxMessages = 20
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("memory_expand: parse args: %w", err)
	}

	convID := ConversationIDFrom(ctx)
	if convID == "" {
		return "", fmt.Errorf("memory_expand: no conversation ID in context")
	}

	if t.deps.SummaryStore == nil {
		return "", fmt.Errorf("memory_expand: summary store not available")
	}

	node, err := t.deps.SummaryStore.GetByConvID(ctx, convID, args.SummaryID)
	if err != nil {
		return "", fmt.Errorf("memory_expand: %w", err)
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

	// Leaf node: return original messages.
	return t.expandLeaf(ctx, convID, node, args.MaxMessages)
}

func (t *memoryExpandTool) expandLeaf(ctx context.Context, convID string, node *SummaryNode, maxMsgs int) (string, error) {
	startSeq := node.EarliestSeq
	endSeq := node.LatestSeq + 1

	// Check archive manifest for Hot/Cold boundary.
	if t.deps.Workspace != nil {
		manifest, err := LoadManifest(ctx, t.deps.Workspace, t.deps.Prefix, convID)
		if err == nil && manifest.HotStartSeq > 0 {
			var allMsgs []model.Message

			// Cold part.
			if startSeq < manifest.HotStartSeq {
				coldEnd := endSeq
				if coldEnd > manifest.HotStartSeq {
					coldEnd = manifest.HotStartSeq
				}
				coldMsgs, err := LoadArchivedMessages(ctx, t.deps.Workspace, t.deps.Prefix, convID, startSeq, coldEnd-1)
				if err == nil {
					allMsgs = append(allMsgs, coldMsgs...)
				}
			}

			// Hot part.
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

	// No archive or fallback: use RangeReader directly.
	if rr, ok := t.deps.MessageStore.(RangeReader); ok {
		msgs, err := rr.GetMessageRange(ctx, convID, startSeq, endSeq)
		if err == nil {
			if len(msgs) > maxMsgs {
				msgs = msgs[len(msgs)-maxMsgs:]
			}
			return formatMessages(msgs), nil
		}
	}

	// Final fallback: get all messages and slice.
	msgs, err := t.deps.MessageStore.GetMessages(ctx, convID)
	if err != nil {
		return "", fmt.Errorf("memory_expand: get messages: %w", err)
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

// --- memory_compact ---

type memoryCompactTool struct {
	deps ToolDeps
}

func newMemoryCompactTool(deps ToolDeps) tool.Tool {
	return &memoryCompactTool{deps: deps}
}

func (t *memoryCompactTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "memory_compact",
		Description: "Manually trigger compact and archive for a conversation's memory DAG.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"conversation_id": map[string]any{
					"type":        "string",
					"description": "The conversation ID to compact/archive",
				},
				"compact": map[string]any{
					"type":        "boolean",
					"description": "Run DAG compact",
					"default":     true,
				},
				"archive": map[string]any{
					"type":        "boolean",
					"description": "Run message archiving",
					"default":     true,
				},
			},
			"required": []string{"conversation_id"},
		},
	}
}

func (t *memoryCompactTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args struct {
		ConversationID string `json:"conversation_id"`
		Compact        *bool  `json:"compact"`
		Archive        *bool  `json:"archive"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("memory_compact: parse args: %w", err)
	}

	doCompact := args.Compact == nil || *args.Compact
	doArchive := args.Archive == nil || *args.Archive

	type resultJSON struct {
		CompactResult *CompactResult `json:"compact_result,omitempty"`
		ArchiveResult *ArchiveResult `json:"archive_result,omitempty"`
	}
	var res resultJSON

	// We need a SummaryDAG reference. The tool deps don't directly hold it.
	// Instead, we operate through the store interfaces.
	cfg := t.deps.Config
	if doCompact && t.deps.SummaryStore != nil {
		dag := &SummaryDAG{
			store:  t.deps.SummaryStore,
			config: cfg,
		}
		cr, err := dag.Compact(ctx, args.ConversationID)
		if err != nil {
			return "", fmt.Errorf("memory_compact: compact: %w", err)
		}
		res.CompactResult = &cr
	}

	if doArchive && t.deps.Workspace != nil && t.deps.MessageStore != nil {
		ar, err := Archive(ctx, t.deps.Workspace, t.deps.MessageStore, t.deps.Prefix, args.ConversationID, cfg.Archive)
		if err != nil {
			return "", fmt.Errorf("memory_compact: archive: %w", err)
		}
		res.ArchiveResult = &ar
	}

	data, _ := json.Marshal(res)
	return string(data), nil
}
