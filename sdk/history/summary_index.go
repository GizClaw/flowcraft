package history

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// BuildSummaryIndex generates a summary index string from the top-level
// summaries of a conversation. The result is intended to be injected into
// the LLM system prompt via the workflow.VarSummaryIndex board variable.
//
// Returns an empty string when no summaries exist or the store is nil.
// The budget parameter controls the maximum character length of the output;
// older summaries are omitted (with a note) when the budget is exceeded.
func BuildSummaryIndex(ctx context.Context, store SummaryStore, convID string, budget int) string {
	if store == nil || convID == "" {
		return ""
	}

	nodes, err := topLevelSummaries(ctx, store, convID)
	if err != nil || len(nodes) == 0 {
		return ""
	}

	return formatSummaryIndex(nodes, budget)
}

// topLevelSummaries returns the highest-depth active summary nodes for a
// conversation, sorted by EarliestSeq ascending.
func topLevelSummaries(ctx context.Context, store SummaryStore, convID string) ([]*SummaryNode, error) {
	all, err := store.ListAll(ctx, convID)
	if err != nil {
		return nil, err
	}

	deleted := make(map[string]bool)
	latest := make(map[string]*SummaryNode)
	maxDepth := 0

	for _, n := range all {
		if n.Deleted {
			deleted[n.ID] = true
			continue
		}
		deleted[n.ID] = false
		latest[n.ID] = n
		if n.Depth > maxDepth {
			maxDepth = n.Depth
		}
	}

	var top []*SummaryNode
	for id, isDel := range deleted {
		if isDel {
			continue
		}
		n := latest[id]
		if n.Depth == maxDepth {
			top = append(top, n)
		}
	}

	sort.Slice(top, func(i, j int) bool {
		return top[i].EarliestSeq < top[j].EarliestSeq
	})

	return top, nil
}

// formatSummaryIndex renders summary nodes into a human-readable index.
// It fills from the newest summary backwards; when the budget is exceeded,
// earlier entries are dropped with a note.
func formatSummaryIndex(nodes []*SummaryNode, budget int) string {
	if budget <= 0 {
		budget = 1500
	}

	const header = "## Conversation Summary\n\nBelow are compressed summaries of earlier conversation. To view the original messages, call history_expand(summary_id=ID).\n\n"

	lines := make([]string, len(nodes))
	for i, n := range nodes {
		content := truncateSummary(n.Content, 200)
		lines[i] = fmt.Sprintf("[%s] seq %d-%d: %s", n.ID, n.EarliestSeq, n.LatestSeq, content)
	}

	remaining := budget - len(header)
	if remaining <= 0 {
		return ""
	}

	var included []string
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		lineLen := len(lines[i]) + 1 // +1 for newline
		if total+lineLen > remaining {
			break
		}
		included = append([]string{lines[i]}, included...)
		total += lineLen
	}

	if len(included) == 0 {
		return ""
	}

	omitted := len(nodes) - len(included)
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		fmt.Fprintf(&b, "(%d earlier summaries omitted)\n", omitted)
	}
	for _, line := range included {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	return b.String()
}

func truncateSummary(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
